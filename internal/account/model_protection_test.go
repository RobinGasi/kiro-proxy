package account

import (
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RobinGasi/kiro-proxy/internal/config"
)

func TestModelProtectionPersistsAndDoesNotBlockOtherModels(t *testing.T) {
	stateFile := filepath.Join(t.TempDir(), "state.json")
	newManager := func() (*Manager, *Account) {
		acct := &Account{id: "default"}
		manager := &Manager{
			cfg: &config.Config{
				AccountsStateFile:               stateFile,
				ModelRestrictionCooldownSeconds: 3600,
			},
			log:      slog.New(slog.DiscardHandler),
			accounts: []*Account{acct},
			byID:     map[string]*Account{"default": acct},
		}
		return manager, acct
	}

	manager, acct := newManager()
	protection := manager.ProtectModelAfterEmptyResponse(acct, "claude-opus-5.5")
	if protection == nil || protection.RetryAfterSeconds() <= 0 {
		t.Fatalf("protection = %#v, want active cooldown", protection)
	}
	if !strings.Contains(protection.Error(), "use another model") {
		t.Fatalf("error = %q, want actionable model-switch guidance", protection)
	}
	if got := manager.ModelProtection(acct, "gpt-5.6-sol"); got != nil {
		t.Fatalf("other model protection = %#v, want nil", got)
	}
	if err := manager.SaveState(); err != nil {
		t.Fatalf("save state: %v", err)
	}

	restarted, restartedAcct := newManager()
	if err := restarted.LoadState(); err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got := restarted.ModelProtection(restartedAcct, "claude-opus-5.5"); got == nil {
		t.Fatal("model protection was lost across restart")
	}
}

func TestModelProtectionAllowsSingleHalfOpenProbe(t *testing.T) {
	acct := &Account{
		id: "default",
		modelProtections: map[string]modelProtectionState{
			"claude-opus-5.5": {Until: time.Now().Add(-time.Second)},
		},
	}
	manager := &Manager{
		cfg: &config.Config{ModelRestrictionCooldownSeconds: 3600},
		log: slog.New(slog.DiscardHandler),
	}

	if got := manager.ModelProtection(acct, "claude-opus-5.5"); got != nil {
		t.Fatalf("first caller after cooldown = %#v, want half-open probe access", got)
	}
	if got := manager.ModelProtection(acct, "claude-opus-5.5"); got == nil {
		t.Fatal("second caller passed while half-open probe was in flight")
	}

	manager.ReleaseModelProbe(acct, "claude-opus-5.5")
	if got := manager.ModelProtection(acct, "claude-opus-5.5"); got != nil {
		t.Fatalf("caller after released probe = %#v, want probe access", got)
	}
	manager.RecordModelSuccess(acct, "claude-opus-5.5")
	if _, ok := acct.modelProtections["claude-opus-5.5"]; ok {
		t.Fatal("successful half-open probe did not close the breaker")
	}
}

func TestSaveStateRemainsDirtyAfterWriteFailure(t *testing.T) {
	manager := &Manager{
		cfg: &config.Config{
			AccountsStateFile: filepath.Join(t.TempDir(), "missing", "state.json"),
		},
		log:   slog.New(slog.DiscardHandler),
		dirty: true,
	}

	if err := manager.SaveState(); err == nil {
		t.Fatal("SaveState succeeded with a missing parent directory")
	}
	if !manager.isDirty() {
		t.Fatal("failed state save cleared dirty state and disabled periodic retry")
	}
}
