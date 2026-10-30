package main

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	server "github.com/recurser/bossd/api/daemon"
)

func TestHandshakeDoesNotWaitForCredentialKeyring(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		loaded := make(chan struct{})
		completed := make(chan struct{})
		devices := server.NewCredentialDeviceService(server.CredentialDeviceConfig{
			IdentityKey: ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)),
			LoadKey: func(string) (*ecdh.PrivateKey, error) {
				close(loaded)
				<-release
				return nil, errors.New("keyring unavailable")
			},
		})
		worker := newCredentialEnrollmentWorker(devices.EnsureEnrollment, func(context.Context) error { close(completed); return nil })
		w := &daemonWiring{credentialDevices: devices, credentialEnrollment: worker}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(ctx) }()
		returned := make(chan struct{})
		go func() { w.onUpstreamHandshake(); close(returned) }()
		synctest.Wait()
		select {
		case <-returned:
		default:
			t.Error("handshake waited on enrollment")
		}
		select {
		case <-loaded:
		default:
			t.Error("enrollment did not reach keyring")
		}
		select {
		case <-completed:
			t.Error("enrollment completed while keyring was blocked")
		default:
		}
		close(release)
		<-completed
		cancel()
		<-done
	})
}

func TestCredentialEnrollmentRetriesFailuresInOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls []string
		worker := newCredentialEnrollmentWorker(func(context.Context) error {
			calls = append(calls, "enroll")
			if len(calls) == 1 {
				return errors.New("enrollment failed")
			}
			return nil
		}, func(context.Context) error {
			calls = append(calls, "reconnect")
			return errors.New("reconnect failed")
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(ctx) }()
		worker.Nudge()
		synctest.Wait()
		worker.Nudge()
		synctest.Wait()
		if want := []string{"enroll", "reconnect", "enroll", "reconnect"}; !reflect.DeepEqual(calls, want) {
			t.Errorf("calls = %v, want %v", calls, want)
		}
		cancel()
		<-done
	})
}

func TestCredentialEnrollmentCoalescesInFlightNudges(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		calls := 0
		worker := newCredentialEnrollmentWorker(func(context.Context) error {
			calls++
			if calls == 1 {
				<-release
			}
			return nil
		}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(ctx) }()
		worker.Nudge()
		synctest.Wait()
		worker.Nudge()
		worker.Nudge()
		close(release)
		synctest.Wait()
		if calls != 2 {
			t.Errorf("enrollment passes = %d, want 2", calls)
		}
		cancel()
		<-done
	})
}

func TestCredentialEnrollmentShutdown(t *testing.T) {
	for _, cancellable := range []bool{true, false} {
		name := "keyring"
		if cancellable {
			name = "rpc"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				observed := make(chan error, 1)
				worker := newCredentialEnrollmentWorker(func(ctx context.Context) error {
					if cancellable {
						<-ctx.Done()
						observed <- ctx.Err()
					} else {
						<-release
					}
					return nil
				}, nil)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan struct{})
				go func() { defer close(done); worker.Run(ctx) }()
				worker.Nudge()
				synctest.Wait()
				cancel()
				synctest.Wait()
				if cancellable {
					if err := <-observed; !errors.Is(err, context.Canceled) {
						t.Errorf("RPC cancellation = %v", err)
					}
				} else {
					select {
					case <-done:
						t.Error("Run abandoned an in-flight keyring call")
					default:
					}
					close(release)
				}
				<-done
			})
		})
	}
}

func TestCredentialEnrollmentPassTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		observed := make(chan error, 1)
		worker := newCredentialEnrollmentWorker(func(ctx context.Context) error { <-ctx.Done(); observed <- ctx.Err(); return ctx.Err() }, nil)
		worker.timeout = time.Second
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(ctx) }()
		worker.Nudge()
		if err := <-observed; !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("pass timeout = %v", err)
		}
		cancel()
		<-done
	})
}
