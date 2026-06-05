package main

import (
	"errors"
	"os"
	"reflect"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/recurser/bossalib/config"
)

func TestBossdPgrepArgsRestrictsToEffectiveUser(t *testing.T) {
	got := bossdPgrepArgs()
	want := []string{"-u", strconv.Itoa(os.Geteuid()), "-x", "bossd"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bossdPgrepArgs() = %v, want %v", got, want)
	}
}

func TestRestartSocketPath(t *testing.T) {
	t.Run("returns path", func(t *testing.T) {
		got, err := restartSocketPath("/tmp/boss.sock", nil)
		if err != nil {
			t.Fatalf("restartSocketPath returned error: %v", err)
		}
		if got != "/tmp/boss.sock" {
			t.Fatalf("restartSocketPath returned %q, want /tmp/boss.sock", got)
		}
	})

	t.Run("surfaces path error", func(t *testing.T) {
		pathErr := errors.New("home unavailable")
		_, err := restartSocketPath("", pathErr)
		if !errors.Is(err, pathErr) {
			t.Fatalf("restartSocketPath error = %v, want %v", err, pathErr)
		}
	})

	t.Run("rejects empty path", func(t *testing.T) {
		_, err := restartSocketPath("", nil)
		if err == nil {
			t.Fatal("restartSocketPath returned nil error for empty path")
		}
	})
}

func TestLaunchSettingsDoesNotSaveInstalledAtWhenLoadFails(t *testing.T) {
	oldLoadSettings := loadSettings
	oldSaveSettings := saveSettings
	defer func() {
		loadSettings = oldLoadSettings
		saveSettings = oldSaveSettings
	}()

	settings := config.DefaultSettings()
	settings.BossCloudGuestOfferHidden = true
	loadSettings = func() (config.Settings, error) {
		return settings, errors.New("corrupt settings")
	}
	saveSettings = func(config.Settings) error {
		t.Fatal("saveSettings called after load error")
		return nil
	}

	got := launchSettings(time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC))

	if !got.BossCloudGuestOfferHidden {
		t.Fatal("launchSettings did not return loaded runtime settings")
	}
}

func TestLaunchSettingsSavesWhenInstalledAtMissing(t *testing.T) {
	oldLoadSettings := loadSettings
	oldSaveSettings := saveSettings
	defer func() {
		loadSettings = oldLoadSettings
		saveSettings = oldSaveSettings
	}()

	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.FixedZone("JST", 9*60*60))
	settings := config.DefaultSettings()
	loadSettings = func() (config.Settings, error) {
		return settings, nil
	}
	var saved config.Settings
	saveCalls := 0
	saveSettings = func(s config.Settings) error {
		saveCalls++
		saved = s
		return nil
	}

	got := launchSettings(now)

	if saveCalls != 1 {
		t.Fatalf("saveSettings calls = %d, want 1", saveCalls)
	}
	if !saved.InstalledAt.Equal(now.UTC()) {
		t.Fatalf("saved InstalledAt = %s, want %s", saved.InstalledAt, now.UTC())
	}
	if !got.InstalledAt.Equal(now.UTC()) {
		t.Fatalf("returned InstalledAt = %s, want %s", got.InstalledAt, now.UTC())
	}
}

func TestParsePgrepOutput(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []int
	}{
		{
			name: "single PID",
			in:   "12345\n",
			want: []int{12345},
		},
		{
			name: "multiple PIDs",
			in:   "100\n200\n300\n",
			want: []int{100, 200, 300},
		},
		{
			name: "empty pgrep output",
			in:   "",
			want: nil,
		},
		{
			name: "blank trailing lines tolerated",
			in:   "\n\n42\n\n",
			want: []int{42},
		},
		{
			name: "non-numeric lines skipped",
			in:   "42\nnot a pid\n99\n",
			want: []int{42, 99},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parsePgrepOutput(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parsePgrepOutput(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

type fakeProcess struct {
	err error
}

func (p fakeProcess) Signal(os.Signal) error {
	return p.err
}

func TestSignalBossdProcessesCountsOnlySuccessfulSignals(t *testing.T) {
	got, err := signalBossdProcesses([]int{100, 200, 300}, func(pid int) (processSignaler, error) {
		switch pid {
		case 100:
			return fakeProcess{}, nil
		case 200:
			return fakeProcess{err: syscall.ESRCH}, nil
		case 300:
			return fakeProcess{err: syscall.EPERM}, nil
		default:
			t.Fatalf("unexpected pid %d", pid)
			return fakeProcess{}, nil
		}
	})

	if got != 1 {
		t.Fatalf("signalBossdProcesses signalled %d processes, want 1", got)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("signalBossdProcesses error = %v, want EPERM", err)
	}
}

func TestSignalBossdProcessesSurfacesFindFailures(t *testing.T) {
	findErr := errors.New("missing process")
	got, err := signalBossdProcesses([]int{100}, func(int) (processSignaler, error) {
		return nil, findErr
	})

	if got != 0 {
		t.Fatalf("signalBossdProcesses signalled %d processes, want 0", got)
	}
	if !errors.Is(err, findErr) {
		t.Fatalf("signalBossdProcesses error = %v, want %v", err, findErr)
	}
}
