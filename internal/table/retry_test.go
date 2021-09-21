package table

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"testing"
	"time"

	"github.com/ydb-platform/ydb-go-sdk/v3/internal/errors"
	"github.com/ydb-platform/ydb-go-sdk/v3/trace"

	"github.com/ydb-platform/ydb-go-genproto/protos/Ydb_Table"
	"google.golang.org/protobuf/proto"

	"github.com/ydb-platform/ydb-go-sdk/v3/testutil"
)

func TestRetryerBackoffRetryCancelation(t *testing.T) {
	for _, testErr := range []error{
		// Errors leading to Wait repeat.
		errors.NewTransportError(
			errors.TransportErrorResourceExhausted,
		),
		fmt.Errorf("wrap transport error: %w", errors.NewTransportError(
			errors.TransportErrorResourceExhausted,
		)),
		errors.NewOpError(errors.WithOEReason(errors.StatusOverloaded)),
		fmt.Errorf("wrap op error: %w", errors.NewOpError(errors.WithOEReason(errors.StatusOverloaded))),
	} {
		t.Run("", func(t *testing.T) {
			backoff := make(chan chan time.Time)
			pool := SingleSession(
				simpleSession(),
				testutil.BackoffFunc(func(n int) <-chan time.Time {
					ch := make(chan time.Time)
					backoff <- ch
					return ch
				}),
			)

			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error)
			go func() {
				result <- pool.Retry(ctx, false, func(ctx context.Context, _ *Session) error {
					return testErr
				})
			}()

			select {
			case <-backoff:
			case err := <-result:
				t.Fatalf("unexpected result: %v", err)
			}

			cancel()
			if err := <-result; !errors.Is(err, testErr) {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestRetryerImmediateRetry(t *testing.T) {
	for testErr, session := range map[error]*Session{
		errors.NewTransportError(
			errors.TransportErrorResourceExhausted,
		): newSession(nil, "1"),
		errors.NewTransportError(
			errors.TransportErrorAborted,
		): newSession(nil, "2"),
		errors.NewOpError(
			errors.WithOEReason(errors.StatusUnavailable),
		): newSession(nil, "3"),
		errors.NewOpError(
			errors.WithOEReason(errors.StatusOverloaded),
		): newSession(nil, "4"),
		errors.NewOpError(
			errors.WithOEReason(errors.StatusAborted),
		): newSession(nil, "5"),
		errors.NewOpError(
			errors.WithOEReason(errors.StatusNotFound),
		): newSession(nil, "6"),
		fmt.Errorf("wrap op error: %w", errors.NewOpError(
			errors.WithOEReason(errors.StatusAborted),
		)): newSession(nil, "7"),
	} {
		t.Run(fmt.Sprintf("err: %v, session: %v", testErr, session != nil), func(t *testing.T) {
			pool := SingleSession(
				simpleSession(),
				testutil.BackoffFunc(func(n int) <-chan time.Time {
					t.Fatalf("this code will not be called")
					return nil
				}),
			)
			err := pool.Retry(
				context.Background(),
				false,
				func(ctx context.Context, _ *Session) error {
					return testErr
				},
			)
			if !errors.Is(err, testErr) {
				t.Fatalf("unexpected error: %v; want: %v", err, testErr)
			}
		})
	}
}

func TestRetryerBadSession(t *testing.T) {
	pool := SessionProviderFunc{
		OnGet: func(ctx context.Context) (*Session, error) {
			return simpleSession(), nil
		},
		OnPut:   nil,
		OnRetry: nil,
	}

	var sessions []*Session
	err := pool.Retry(
		context.Background(),
		false,
		func(ctx context.Context, s *Session) error {
			sessions = append(sessions, s)
			return errors.NewOpError(errors.WithOEReason(errors.StatusBadSession))
		},
	)
	if !errors.IsOpError(err, errors.StatusBadSession) {
		t.Errorf("unexpected error: %v", err)
	}
	seen := make(map[*Session]bool, len(sessions))
	for _, s := range sessions {
		if seen[s] {
			t.Errorf("session used twice")
		} else {
			seen[s] = true
		}
		s.closeMux.Lock()
		if !s.closed {
			t.Errorf("bad session was not closed")
		}
		s.closeMux.Unlock()
	}
}

func TestRetryerBadSessionReuse(t *testing.T) {
	client := &client{
		cluster: testutil.NewDB(testutil.WithInvokeHandlers(testutil.InvokeHandlers{
			testutil.TableCreateSession: func(request interface{}) (result proto.Message, err error) {
				return &Ydb_Table.CreateSessionResult{}, nil
			}})),
	}
	var (
		sessions = make([]*Session, 10)
		bad      = make(map[*Session]bool)
		reused   = make(map[*Session]bool)
	)
	for i := range sessions {
		s, err := client.CreateSession(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sessions[i] = s
		bad[s] = i < len(sessions)-1 // All bad but last.
	}
	var (
		i    int
		pool SessionProvider
	)
	backoff := testutil.BackoffFunc(func(n int) <-chan time.Time {
		t.Fatalf("this code will not be called")
		return nil
	})
	pool = SessionProviderFunc{
		OnGet: func(_ context.Context) (*Session, error) {
			defer func() { i++ }()
			return sessions[i], nil
		},
		OnPut: func(_ context.Context, s *Session) error {
			reused[s] = true
			return nil
		},
		OnRetry: func(ctx context.Context, operation RetryOperation) error {
			return retryBackoff(ctx, pool, backoff, backoff, false, operation)
		},
	}
	_ = pool.Retry(
		context.Background(),
		false,
		func(ctx context.Context, s *Session) error {
			if bad[s] {
				return errors.NewOpError(errors.WithOEReason(errors.StatusBadSession))
			}
			return nil
		},
	)
	for _, s := range sessions {
		if bad[s] && reused[s] {
			t.Errorf("reused bad session")
		}
		if !bad[s] && !reused[s] {
			t.Errorf("missed good session")
		}
	}
}

func TestRetryerImmediateReturn(t *testing.T) {
	for _, testErr := range []error{
		errors.NewOpError(
			errors.WithOEReason(errors.StatusGenericError),
		),
		fmt.Errorf("wrap op error: %w", errors.NewOpError(
			errors.WithOEReason(errors.StatusGenericError),
		)),
		errors.NewTransportError(
			errors.TransportErrorPermissionDenied,
		),
		fmt.Errorf("wrap transport error: %w", errors.NewTransportError(
			errors.TransportErrorPermissionDenied,
		)),
		errors.New("whoa"),
	} {
		t.Run("", func(t *testing.T) {
			defer func() {
				if e := recover(); e != nil {
					t.Fatalf("unexpected panic: %v", e)
				}
			}()
			pool := SingleSession(
				simpleSession(),
				testutil.BackoffFunc(func(n int) <-chan time.Time {
					panic("this code will not be called")
				}),
			)
			err := pool.Retry(
				context.Background(),
				false,
				func(ctx context.Context, _ *Session) error {
					return testErr
				},
			)
			if !errors.Is(err, testErr) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// We are testing all suspentions of custom operation func against to all context
// timeouts - all sub-tests must have latency less than timeouts (+tolerance)
func TestRetryContextDeadline(t *testing.T) {
	tolerance := 10 * time.Millisecond
	timeouts := []time.Duration{
		50 * time.Millisecond,
		100 * time.Millisecond,
		200 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
	}
	sleeps := []time.Duration{
		time.Nanosecond,
		time.Microsecond,
		time.Millisecond,
		10 * time.Millisecond,
		50 * time.Millisecond,
		100 * time.Millisecond,
		500 * time.Millisecond,
		time.Second,
		5 * time.Second,
	}
	errs := []error{
		io.EOF,
		context.DeadlineExceeded,
		fmt.Errorf("test error"),
		errors.NewTransportError(
			errors.TransportErrorUnknownCode,
		),
		errors.NewTransportError(
			errors.TransportErrorCanceled,
		),
		errors.NewTransportError(
			errors.TransportErrorUnknown,
		),
		errors.NewTransportError(
			errors.TransportErrorInvalidArgument,
		),
		errors.NewTransportError(
			errors.TransportErrorDeadlineExceeded,
		),
		errors.NewTransportError(
			errors.TransportErrorNotFound,
		),
		errors.NewTransportError(
			errors.TransportErrorAlreadyExists,
		),
		errors.NewTransportError(
			errors.TransportErrorPermissionDenied,
		),
		errors.NewTransportError(
			errors.TransportErrorResourceExhausted,
		),
		errors.NewTransportError(
			errors.TransportErrorFailedPrecondition,
		),
		errors.NewTransportError(
			errors.TransportErrorAborted,
		),
		errors.NewTransportError(
			errors.TransportErrorOutOfRange,
		),
		errors.NewTransportError(
			errors.TransportErrorUnimplemented,
		),
		errors.NewTransportError(
			errors.TransportErrorInternal,
		),
		errors.NewTransportError(
			errors.TransportErrorUnavailable,
		),
		errors.NewTransportError(
			errors.TransportErrorDataLoss,
		),
		errors.NewTransportError(
			errors.TransportErrorUnauthenticated,
		),
		errors.NewOpError(errors.WithOEReason(errors.StatusUnknownStatus)),
		errors.NewOpError(errors.WithOEReason(errors.StatusBadRequest)),
		errors.NewOpError(errors.WithOEReason(errors.StatusUnauthorized)),
		errors.NewOpError(errors.WithOEReason(errors.StatusInternalError)),
		errors.NewOpError(errors.WithOEReason(errors.StatusAborted)),
		errors.NewOpError(errors.WithOEReason(errors.StatusUnavailable)),
		errors.NewOpError(errors.WithOEReason(errors.StatusOverloaded)),
		errors.NewOpError(errors.WithOEReason(errors.StatusSchemeError)),
		errors.NewOpError(errors.WithOEReason(errors.StatusGenericError)),
		errors.NewOpError(errors.WithOEReason(errors.StatusTimeout)),
		errors.NewOpError(errors.WithOEReason(errors.StatusBadSession)),
		errors.NewOpError(errors.WithOEReason(errors.StatusPreconditionFailed)),
		errors.NewOpError(errors.WithOEReason(errors.StatusAlreadyExists)),
		errors.NewOpError(errors.WithOEReason(errors.StatusNotFound)),
		errors.NewOpError(errors.WithOEReason(errors.StatusSessionExpired)),
		errors.NewOpError(errors.WithOEReason(errors.StatusCancelled)),
		errors.NewOpError(errors.WithOEReason(errors.StatusUndetermined)),
		errors.NewOpError(errors.WithOEReason(errors.StatusUnsupported)),
		errors.NewOpError(errors.WithOEReason(errors.StatusSessionBusy)),
	}
	client := &client{
		cluster: testutil.NewDB(testutil.WithInvokeHandlers(testutil.InvokeHandlers{})),
	}
	pool := SessionProviderFunc{
		OnGet: client.CreateSession,
	}
	for i := range timeouts {
		for j := range sleeps {
			timeout := timeouts[i]
			sleep := sleeps[j]
			t.Run(fmt.Sprintf("timeout %v, sleep %v", timeout, sleep), func(t *testing.T) {
				random := rand.New(rand.NewSource(time.Now().Unix()))
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				defer cancel()
				_ = pool.Retry(
					trace.WithRetryTrace(
						ctx,
						trace.RetryTrace{
							OnRetry: func(info trace.RetryLoopStartInfo) func(trace.RetryLoopDoneInfo) {
								return func(info trace.RetryLoopDoneInfo) {
									if info.Latency-timeouts[i] > tolerance {
										t.Errorf("unexpected latency: %v (attempts %d)", info.Latency, info.Attempts)
									}
								}
							},
						},
					),
					false,
					func(ctx context.Context, _ *Session) error {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(sleep):
							return errs[random.Intn(len(errs))]
						}
					},
				)
			})
		}
	}
}