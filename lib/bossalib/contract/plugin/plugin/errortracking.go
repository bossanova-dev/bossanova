package plugin

import (
	"context"
	"os"

	"github.com/getsentry/sentry-go"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/errortrack"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
)

// InitErrorTracking enables plugin panic reporting only when bossd supplied a DSN.
// An invalid DSN never prevents plugin startup.
func InitErrorTracking(logger zerolog.Logger, name string) (closeFn func()) {
	return initErrorTracking(logger, name, os.Getenv, nil)
}

func initErrorTracking(logger zerolog.Logger, name string, getenv func(string) string, transport sentry.Transport) func() {
	dsn := getenv(ErrorTrackingDSNEnvVar)
	if dsn == "" {
		return func() {}
	}
	environment := getenv(EnvironmentEnvVar)
	if environment == "" {
		environment = "local"
	}
	closeFn, err := errortrack.Init(errortrack.Opts{
		DSN: dsn, App: "bossd-plugin-" + name, Environment: environment,
		Release: buildinfo.Version + "-" + buildinfo.Commit, Transport: transport,
	})
	if err != nil {
		logger.Warn().Err(err).Msg("errortrack disabled")
		return func() {}
	}
	return closeFn
}

func errorTrackingUnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	defer func() {
		if r := recover(); r != nil {
			errortrack.CaptureFatalPanic(r, info.FullMethod)
			panic(r)
		}
	}()
	return handler(ctx, req)
}

func errorTrackingStreamInterceptor(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	defer func() {
		if r := recover(); r != nil {
			errortrack.CaptureFatalPanic(r, info.FullMethod)
			panic(r)
		}
	}()
	return handler(srv, stream)
}
