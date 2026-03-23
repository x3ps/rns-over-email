package config

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// validArgs returns CLI args that produce a valid config (all required fields).
func validArgs() []string {
	return []string{
		"--smtp-host", "smtp.test.com",
		"--smtp-username", "user",
		"--smtp-password", "pass",
		"--smtp-from", "user@test.com",
		"--imap-host", "imap.test.com",
		"--imap-username", "imapuser",
		"--imap-password", "imappass",
		"--peer-email", "peer@test.com",
	}
}

// validConfig returns a Config that passes validation, for use in mutation tests.
func validConfig() Config {
	cfg := Defaults()
	Normalize(&cfg)
	cfg.SMTP.Host = "smtp.test.com"
	cfg.SMTP.Username = "user"
	cfg.SMTP.Password = "pass"
	cfg.SMTP.From = "user@test.com"
	cfg.IMAP.Host = "imap.test.com"
	cfg.IMAP.Username = "imapuser"
	cfg.IMAP.Password = "imappass"
	cfg.Peer.Email = "peer@test.com"
	cfg.Checkpoint.Path = DeriveCheckpointPath(cfg.Pipe.Name)
	return cfg
}

func TestDefaults(t *testing.T) {
	cfg := Defaults()

	if cfg.Pipe.Name != "EmailTransport" {
		t.Errorf("pipe.name = %q, want EmailTransport", cfg.Pipe.Name)
	}
	if cfg.Pipe.MTU != 500 {
		t.Errorf("pipe.mtu = %d, want 500", cfg.Pipe.MTU)
	}
	if cfg.SMTP.Port != 587 {
		t.Errorf("smtp.port = %d, want 587", cfg.SMTP.Port)
	}
	if cfg.SMTP.TLS != "starttls" {
		t.Errorf("smtp.tls = %q, want starttls", cfg.SMTP.TLS)
	}
	if cfg.IMAP.Port != 993 {
		t.Errorf("imap.port = %d, want 993", cfg.IMAP.Port)
	}
	if cfg.IMAP.TLS != "tls" {
		t.Errorf("imap.tls = %q, want tls", cfg.IMAP.TLS)
	}
	if cfg.IMAP.PollInterval.Duration != 60*time.Second {
		t.Errorf("imap.poll_interval = %v, want 60s", cfg.IMAP.PollInterval)
	}
	if cfg.Checkpoint.Path != "" {
		t.Errorf("checkpoint.path = %q, want empty (sentinel)", cfg.Checkpoint.Path)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("logging.level = %q, want info", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("logging.format = %q, want text", cfg.Logging.Format)
	}
}

func TestFlagOverrides(t *testing.T) {
	args := append(validArgs(), "--pipe-name", "FlagPipe", "--pipe-mtu", "750")
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Pipe.Name != "FlagPipe" {
		t.Errorf("pipe.name = %q, want FlagPipe", cfg.Pipe.Name)
	}
	if cfg.Pipe.MTU != 750 {
		t.Errorf("pipe.mtu = %d, want 750", cfg.Pipe.MTU)
	}
	// Defaults preserved where not overridden.
	if cfg.IMAP.Port != 993 {
		t.Errorf("imap.port = %d, want 993 (default)", cfg.IMAP.Port)
	}
}

func TestAllFlags(t *testing.T) {
	args := []string{
		"--pipe-name", "AllFlags",
		"--pipe-mtu", "300",
		"--smtp-host", "smtp.all.com",
		"--smtp-port", "465",
		"--smtp-username", "suser",
		"--smtp-password", "spass",
		"--smtp-from", "s@all.com",
		"--smtp-tls", "tls",
		"--imap-host", "imap.all.com",
		"--imap-port", "143",
		"--imap-username", "iuser",
		"--imap-password", "ipass",
		"--imap-folder", "RNS",
		"--imap-tls", "starttls",
		"--imap-poll-interval", "30s",
		"--imap-cleanup-mode", "delete",
		"--peer-email", "p@all.com",
		"--checkpoint-path", "/tmp/cp.json",
		"--log-level", "debug",
		"--log-format", "json",
	}
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Pipe.Name != "AllFlags" {
		t.Errorf("pipe.name = %q", cfg.Pipe.Name)
	}
	if cfg.SMTP.Port != 465 {
		t.Errorf("smtp.port = %d", cfg.SMTP.Port)
	}
	if cfg.IMAP.Folder != "RNS" {
		t.Errorf("imap.folder = %q", cfg.IMAP.Folder)
	}
	if cfg.IMAP.PollInterval.Duration != 30*time.Second {
		t.Errorf("imap.poll_interval = %v", cfg.IMAP.PollInterval)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("logging.format = %q", cfg.Logging.Format)
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("RNS_EMAIL_PIPE_NAME", "EnvPipe")
	t.Setenv("RNS_EMAIL_PIPE_MTU", "750")

	cfg, err := Load(validArgs())
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Pipe.Name != "EnvPipe" {
		t.Errorf("pipe.name = %q, want EnvPipe (env override)", cfg.Pipe.Name)
	}
	if cfg.Pipe.MTU != 750 {
		t.Errorf("pipe.mtu = %d, want 750 (env override)", cfg.Pipe.MTU)
	}
}

func TestFlagsOverrideEnv(t *testing.T) {
	t.Setenv("RNS_EMAIL_PIPE_NAME", "EnvPipe")

	args := append(validArgs(), "--pipe-name", "FlagPipe")
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Pipe.Name != "FlagPipe" {
		t.Errorf("pipe.name = %q, want FlagPipe (flag override)", cfg.Pipe.Name)
	}
}

func TestPasswordFile(t *testing.T) {
	dir := t.TempDir()
	smtpPWFile := filepath.Join(dir, "smtp-pw")
	imapPWFile := filepath.Join(dir, "imap-pw")
	if err := os.WriteFile(smtpPWFile, []byte("file-smtp-pw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imapPWFile, []byte("file-imap-pw\r\n"), 0600); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"--smtp-host", "smtp.test.com",
		"--smtp-username", "user",
		"--smtp-password", "cli-pw",
		"--smtp-password-file", smtpPWFile,
		"--smtp-from", "user@test.com",
		"--imap-host", "imap.test.com",
		"--imap-username", "imapuser",
		"--imap-password", "cli-pw",
		"--imap-password-file", imapPWFile,
		"--peer-email", "peer@test.com",
	}

	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SMTP.Password != "file-smtp-pw" {
		t.Errorf("smtp.password = %q, want file-smtp-pw (password-file overrides flag)", cfg.SMTP.Password)
	}
	if cfg.IMAP.Password != "file-imap-pw" {
		t.Errorf("imap.password = %q, want file-imap-pw (password-file overrides flag)", cfg.IMAP.Password)
	}
}

func TestPasswordFileViaEnv(t *testing.T) {
	dir := t.TempDir()
	smtpPWFile := filepath.Join(dir, "smtp-pw")
	imapPWFile := filepath.Join(dir, "imap-pw")
	if err := os.WriteFile(smtpPWFile, []byte("env-file-smtp-pw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imapPWFile, []byte("env-file-imap-pw\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RNS_EMAIL_SMTP_PASSWORD_FILE", smtpPWFile)
	t.Setenv("RNS_EMAIL_IMAP_PASSWORD_FILE", imapPWFile)

	// No --smtp-password or --imap-password flags; passwords come from env-specified files.
	args := []string{
		"--smtp-host", "smtp.test.com",
		"--smtp-username", "user",
		"--smtp-from", "user@test.com",
		"--imap-host", "imap.test.com",
		"--imap-username", "imapuser",
		"--peer-email", "peer@test.com",
	}
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.Password != "env-file-smtp-pw" {
		t.Errorf("smtp.password = %q, want env-file-smtp-pw", cfg.SMTP.Password)
	}
	if cfg.IMAP.Password != "env-file-imap-pw" {
		t.Errorf("imap.password = %q, want env-file-imap-pw", cfg.IMAP.Password)
	}
}

func TestPasswordFileFlagOverridesEnvFile(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, "env-pw")
	flagFile := filepath.Join(dir, "flag-pw")
	if err := os.WriteFile(envFile, []byte("env-file-pw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(flagFile, []byte("flag-file-pw\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RNS_EMAIL_SMTP_PASSWORD_FILE", envFile)

	args := append(validArgs(), "--smtp-password-file", flagFile)
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.Password != "flag-file-pw" {
		t.Errorf("smtp.password = %q, want flag-file-pw (flag overrides env)", cfg.SMTP.Password)
	}
}

func TestPasswordFileOverridesEnv(t *testing.T) {
	dir := t.TempDir()
	smtpPWFile := filepath.Join(dir, "smtp-pw")
	if err := os.WriteFile(smtpPWFile, []byte("file-pw\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RNS_EMAIL_SMTP_PASSWORD", "env-pw")

	args := append(validArgs(), "--smtp-password-file", smtpPWFile)
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SMTP.Password != "file-pw" {
		t.Errorf("smtp.password = %q, want file-pw (password-file overrides env)", cfg.SMTP.Password)
	}
}

func TestNormalization(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		check func(*Config) string
	}{
		{
			"smtp tls mixed case",
			append(validArgs(), "--smtp-tls", "StartTLS"),
			func(c *Config) string {
				if c.SMTP.TLS != "starttls" {
					return "smtp.tls = " + c.SMTP.TLS
				}
				return ""
			},
		},
		{
			"imap tls trailing space",
			append(validArgs(), "--imap-tls", " TLS "),
			func(c *Config) string {
				if c.IMAP.TLS != "tls" {
					return "imap.tls = " + c.IMAP.TLS
				}
				return ""
			},
		},
		{
			"log level uppercase",
			append(validArgs(), "--log-level", "DEBUG"),
			func(c *Config) string {
				if c.Logging.Level != "debug" {
					return "logging.level = " + c.Logging.Level
				}
				return ""
			},
		},
		{
			"log format uppercase",
			append(validArgs(), "--log-format", "JSON"),
			func(c *Config) string {
				if c.Logging.Format != "json" {
					return "logging.format = " + c.Logging.Format
				}
				return ""
			},
		},
		{
			"cleanup mode uppercase",
			append(validArgs(), "--imap-cleanup-mode", "DELETE"),
			func(c *Config) string {
				if c.IMAP.Cleanup.Mode != "delete" {
					return "imap.cleanup.mode = " + c.IMAP.Cleanup.Mode
				}
				return ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if msg := tt.check(cfg); msg != "" {
				t.Errorf("normalization failed: %s", msg)
			}
		})
	}
}

func TestTLSEnvNormalization(t *testing.T) {
	t.Setenv("RNS_EMAIL_SMTP_TLS", "TLS")
	t.Setenv("RNS_EMAIL_IMAP_TLS", "STARTTLS")

	cfg, err := Load(validArgs())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.TLS != "tls" {
		t.Errorf("smtp.tls = %q, want tls", cfg.SMTP.TLS)
	}
	if cfg.IMAP.TLS != "starttls" {
		t.Errorf("imap.tls = %q, want starttls", cfg.IMAP.TLS)
	}
}

func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Config)
	}{
		{"empty pipe name", func(c *Config) { c.Pipe.Name = "" }},
		{"zero mtu", func(c *Config) { c.Pipe.MTU = 0 }},
		{"negative mtu", func(c *Config) { c.Pipe.MTU = -1 }},
		{"invalid smtp port", func(c *Config) { c.SMTP.Port = 0 }},
		{"missing smtp host", func(c *Config) { c.SMTP.Host = "" }},
		{"missing smtp username", func(c *Config) { c.SMTP.Username = "" }},
		{"missing smtp password", func(c *Config) { c.SMTP.Password = "" }},
		{"missing smtp from", func(c *Config) { c.SMTP.From = "" }},
		{"invalid smtp tls", func(c *Config) { c.SMTP.TLS = "invalid" }},
		{"invalid imap port", func(c *Config) { c.IMAP.Port = 99999 }},
		{"missing imap username", func(c *Config) { c.IMAP.Username = "" }},
		{"missing imap password", func(c *Config) { c.IMAP.Password = "" }},
		{"empty imap folder", func(c *Config) { c.IMAP.Folder = "" }},
		{"empty checkpoint path", func(c *Config) { c.Checkpoint.Path = "" }},
		{"invalid log level", func(c *Config) { c.Logging.Level = "trace" }},
		{"invalid log format", func(c *Config) { c.Logging.Format = "xml" }},
		{"zero poll interval", func(c *Config) { c.IMAP.PollInterval = Duration{0} }},
		{"missing imap host", func(c *Config) { c.IMAP.Host = "" }},
		{"invalid imap tls", func(c *Config) { c.IMAP.TLS = "invalid" }},
		{"missing peer email", func(c *Config) { c.Peer.Email = "" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(&cfg)
			if err := Validate(&cfg); err == nil {
				t.Error("expected validation error, got nil")
			}
		})
	}
}

func TestEmailFormatValidation(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Config)
	}{
		{"peer email missing @", func(c *Config) { c.Peer.Email = "notanemail" }},
		{"smtp from missing @", func(c *Config) { c.SMTP.From = "notanemail" }},
		// Cases that pass the old @-check but are rejected by net/mail.ParseAddress.
		{"peer email double @", func(c *Config) { c.Peer.Email = "a@@b.com" }},
		{"peer email leading @", func(c *Config) { c.Peer.Email = "@bar.com" }},
		{"peer email trailing @", func(c *Config) { c.Peer.Email = "foo@" }},
		{"smtp from double @", func(c *Config) { c.SMTP.From = "a@@b.com" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(&cfg)
			if err := Validate(&cfg); err == nil {
				t.Error("expected validation error for bad email format, got nil")
			}
		})
	}
}

func TestCRLFInjectionValidation(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Config)
	}{
		{"peer email with CR", func(c *Config) { c.Peer.Email = "user@example.com\r" }},
		{"peer email with LF", func(c *Config) { c.Peer.Email = "user@example.com\n" }},
		{"peer email with CRLF", func(c *Config) { c.Peer.Email = "user@example.com\r\nBCC: attacker@evil.com" }},
		{"peer email with null byte", func(c *Config) { c.Peer.Email = "user@example.com\x00" }},
		{"smtp from with CR", func(c *Config) { c.SMTP.From = "user@example.com\r" }},
		{"smtp from with LF", func(c *Config) { c.SMTP.From = "user@example.com\n" }},
		{"smtp from with null byte", func(c *Config) { c.SMTP.From = "user@example.com\x00" }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(&cfg)
			if err := Validate(&cfg); err == nil {
				t.Error("expected validation error for control characters in email, got nil")
			}
		})
	}
}

func TestCleanupTargetSameAsSource(t *testing.T) {
	cfg := validConfig()
	cfg.IMAP.Cleanup.Mode = "move"
	cfg.IMAP.Cleanup.TargetFolder = cfg.IMAP.Folder // same as source
	if err := Validate(&cfg); err == nil {
		t.Error("expected validation error when target_folder == folder, got nil")
	}
}

func TestHostNormalization(t *testing.T) {
	args := append(validArgs(), "--smtp-host", " SMTP.Test.COM ", "--imap-host", " IMAP.Test.COM ")
	// Override the validArgs hosts
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.Host != "smtp.test.com" {
		t.Errorf("smtp.host = %q, want smtp.test.com", cfg.SMTP.Host)
	}
	if cfg.IMAP.Host != "imap.test.com" {
		t.Errorf("imap.host = %q, want imap.test.com", cfg.IMAP.Host)
	}
}

func TestRequiredFieldsValidation(t *testing.T) {
	// Load with no args at all should fail validation (missing required fields).
	_, err := Load(nil)
	if err == nil {
		t.Error("expected validation error with no args, got nil")
	}
}

func TestInvalidEnvMTU(t *testing.T) {
	t.Setenv("RNS_EMAIL_PIPE_MTU", "abc")
	_, err := Load(validArgs())
	if err == nil {
		t.Error("expected error for invalid RNS_EMAIL_PIPE_MTU, got nil")
	}
}

func TestInvalidEnvPort(t *testing.T) {
	t.Setenv("RNS_EMAIL_SMTP_PORT", "xyz")
	_, err := Load(validArgs())
	if err == nil {
		t.Error("expected error for invalid RNS_EMAIL_SMTP_PORT, got nil")
	}
}

func TestInvalidEnvIMAPPort(t *testing.T) {
	t.Setenv("RNS_EMAIL_IMAP_PORT", "not-a-number")
	_, err := Load(validArgs())
	if err == nil {
		t.Error("expected error for invalid RNS_EMAIL_IMAP_PORT, got nil")
	}
}

func TestInvalidEnvPollInterval(t *testing.T) {
	t.Setenv("RNS_EMAIL_IMAP_POLL_INTERVAL", "not-a-duration")
	_, err := Load(validArgs())
	if err == nil {
		t.Error("expected error for invalid RNS_EMAIL_IMAP_POLL_INTERVAL, got nil")
	}
}

func TestSMTPRecoveryDelayFlag(t *testing.T) {
	args := append(validArgs(), "--smtp-recovery-delay", "150")
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.RecoveryDelay != 150 {
		t.Errorf("smtp.recovery_delay = %d, want 150", cfg.SMTP.RecoveryDelay)
	}
}

func TestSMTPRecoveryDelayEnv(t *testing.T) {
	t.Setenv("RNS_EMAIL_SMTP_RECOVERY_DELAY", "600")
	cfg, err := Load(validArgs())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.RecoveryDelay != 600 {
		t.Errorf("smtp.recovery_delay = %d, want 600", cfg.SMTP.RecoveryDelay)
	}
}

func TestIMAPReconnectDelayFlag(t *testing.T) {
	args := append(validArgs(), "--imap-reconnect-delay", "30")
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP.ReconnectDelay != 30 {
		t.Errorf("imap.reconnect_delay = %d, want 30", cfg.IMAP.ReconnectDelay)
	}
}

func TestIMAPReconnectDelayEnv(t *testing.T) {
	t.Setenv("RNS_EMAIL_IMAP_RECONNECT_DELAY", "20")
	cfg, err := Load(validArgs())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP.ReconnectDelay != 20 {
		t.Errorf("imap.reconnect_delay = %d, want 20", cfg.IMAP.ReconnectDelay)
	}
}

func TestInvalidEnvIMAPReconnectDelay(t *testing.T) {
	t.Setenv("RNS_EMAIL_IMAP_RECONNECT_DELAY", "5m")
	_, err := Load(validArgs())
	if err == nil {
		t.Error("expected error for non-integer RNS_EMAIL_IMAP_RECONNECT_DELAY, got nil")
	}
}

func TestInvalidEnvSMTPRecoveryDelay(t *testing.T) {
	t.Setenv("RNS_EMAIL_SMTP_RECOVERY_DELAY", "5m")
	_, err := Load(validArgs())
	if err == nil {
		t.Error("expected error for non-integer RNS_EMAIL_SMTP_RECOVERY_DELAY, got nil")
	}
}

func TestSMTPMaxRecoveryDelayFlag(t *testing.T) {
	args := append(validArgs(), "--smtp-max-recovery-delay", "3600")
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.MaxRecoveryDelay != 3600 {
		t.Errorf("smtp.max_recovery_delay = %d, want 3600", cfg.SMTP.MaxRecoveryDelay)
	}
}

func TestSMTPMaxRecoveryDelayEnv(t *testing.T) {
	t.Setenv("RNS_EMAIL_SMTP_MAX_RECOVERY_DELAY", "7200")
	cfg, err := Load(validArgs())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.MaxRecoveryDelay != 7200 {
		t.Errorf("smtp.max_recovery_delay = %d, want 7200", cfg.SMTP.MaxRecoveryDelay)
	}
}

func TestSMTPMaxRecoveryDelayLessThanBase(t *testing.T) {
	args := append(validArgs(), "--smtp-recovery-delay", "600", "--smtp-max-recovery-delay", "60")
	_, err := Load(args)
	if err == nil {
		t.Error("expected validation error when max < base, got nil")
	}
}

func TestIMAPMaxReconnectDelayFlag(t *testing.T) {
	args := append(validArgs(), "--imap-max-reconnect-delay", "120")
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP.MaxReconnectDelay != 120 {
		t.Errorf("imap.max_reconnect_delay = %d, want 120", cfg.IMAP.MaxReconnectDelay)
	}
}

func TestIMAPMaxReconnectDelayEnv(t *testing.T) {
	t.Setenv("RNS_EMAIL_IMAP_MAX_RECONNECT_DELAY", "600")
	cfg, err := Load(validArgs())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.IMAP.MaxReconnectDelay != 600 {
		t.Errorf("imap.max_reconnect_delay = %d, want 600", cfg.IMAP.MaxReconnectDelay)
	}
}

func TestIMAPMaxReconnectDelayLessThanBase(t *testing.T) {
	args := append(validArgs(), "--imap-reconnect-delay", "60", "--imap-max-reconnect-delay", "10")
	_, err := Load(args)
	if err == nil {
		t.Error("expected validation error when max < base, got nil")
	}
}

func TestSlogLevel(t *testing.T) {
	tests := []struct {
		level string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"unknown", slog.LevelInfo}, // default fallback
	}

	for _, tt := range tests {
		t.Run(tt.level, func(t *testing.T) {
			cfg := Defaults()
			cfg.Logging.Level = tt.level
			if got := cfg.SlogLevel(); got != tt.want {
				t.Errorf("SlogLevel(%q) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestWarnInsecureTLS(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	cfg := validConfig()
	cfg.SMTP.TLS = "none"
	cfg.IMAP.TLS = "none"
	WarnInsecureTLS(&cfg, logger)

	output := buf.String()
	if !strings.Contains(output, "smtp.tls=none") {
		t.Error("expected SMTP TLS warning in log output")
	}
	if !strings.Contains(output, "imap.tls=none") {
		t.Error("expected IMAP TLS warning in log output")
	}

	// Verify no warnings when TLS is enabled.
	buf.Reset()
	cfg.SMTP.TLS = "tls"
	cfg.IMAP.TLS = "tls"
	WarnInsecureTLS(&cfg, logger)
	if buf.Len() > 0 {
		t.Errorf("expected no warnings for TLS-enabled config, got: %s", buf.String())
	}
}

func TestEmailDisplayNameNormalization(t *testing.T) {
	cfg := validConfig()
	cfg.SMTP.From = "Sender User <sender@test.com>"
	cfg.Peer.Email = "Peer User <peer@test.com>"
	Normalize(&cfg)

	if cfg.SMTP.From != "sender@test.com" {
		t.Errorf("smtp.from = %q, want sender@test.com", cfg.SMTP.From)
	}
	if cfg.Peer.Email != "peer@test.com" {
		t.Errorf("peer.email = %q, want peer@test.com", cfg.Peer.Email)
	}
}

func TestCleanupMoveMissingTarget(t *testing.T) {
	cfg := validConfig()
	cfg.IMAP.Cleanup.Mode = "move"
	cfg.IMAP.Cleanup.TargetFolder = ""
	if err := Validate(&cfg); err == nil {
		t.Error("expected validation error for move mode without target folder, got nil")
	}
}

func TestPasswordFileNotFound(t *testing.T) {
	args := append(validArgs(), "--smtp-password-file", "/nonexistent/path/pw.txt")
	_, err := Load(args)
	if err == nil {
		t.Error("expected error for non-existent password file, got nil")
	}
}

func TestEnvVarEmptyStringIsIgnored(t *testing.T) {
	// Explicitly setting an env var to "" must not override the default value
	// (documents the `if v != ""` guard in applyEnv).
	t.Setenv("RNS_EMAIL_PIPE_NAME", "")
	cfg, err := Load(validArgs())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pipe.Name != "EmailTransport" {
		t.Errorf("pipe.name = %q, want EmailTransport (empty env var must be ignored)", cfg.Pipe.Name)
	}
}

func TestPasswordFileOverridesDirectPassword_BothSources(t *testing.T) {
	// File wins over env password AND direct flag password (three-way precedence).
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "pw")
	if err := os.WriteFile(pwFile, []byte("file-pw\n"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("RNS_EMAIL_SMTP_PASSWORD", "env-pw")

	// Also pass --smtp-password as a flag.
	args := append(validArgs(), "--smtp-password", "flag-pw", "--smtp-password-file", pwFile)
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.Password != "file-pw" {
		t.Errorf("smtp.password = %q, want file-pw (file must win over env and flag)", cfg.SMTP.Password)
	}
}

func TestSMTPMaxRecoveryEqualToBase(t *testing.T) {
	// max == base is valid (boundary: >= not >).
	args := append(validArgs(), "--smtp-recovery-delay", "300", "--smtp-max-recovery-delay", "300")
	_, err := Load(args)
	if err != nil {
		t.Errorf("expected no error when max == base, got: %v", err)
	}
}

func TestPasswordFileMultiLineUsesFirstLine(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "pw")
	if err := os.WriteFile(pwFile, []byte("first-line\nsecond-line\nthird-line\n"), 0600); err != nil {
		t.Fatal(err)
	}

	args := append(validArgs(), "--smtp-password-file", pwFile)
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.Password != "first-line" {
		t.Errorf("smtp.password = %q, want first-line (only first line should be used)", cfg.SMTP.Password)
	}
}

func TestPasswordFileWindowsLineEnding(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "pw")
	if err := os.WriteFile(pwFile, []byte("my-password\r\n"), 0600); err != nil {
		t.Fatal(err)
	}

	args := append(validArgs(), "--smtp-password-file", pwFile)
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SMTP.Password != "my-password" {
		t.Errorf("smtp.password = %q, want my-password (\\r must be stripped from Windows line ending)", cfg.SMTP.Password)
	}
}

func TestPasswordFileEmpty(t *testing.T) {
	dir := t.TempDir()
	pwFile := filepath.Join(dir, "empty-pw")
	if err := os.WriteFile(pwFile, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"--smtp-host", "smtp.test.com",
		"--smtp-username", "user",
		"--smtp-password-file", pwFile,
		"--smtp-from", "user@test.com",
		"--imap-host", "imap.test.com",
		"--imap-username", "imapuser",
		"--imap-password", "imappass",
		"--peer-email", "peer@test.com",
	}
	_, err := Load(args)
	if err == nil {
		t.Error("expected validation error for empty password file, got nil")
	}
	if err != nil && !strings.Contains(err.Error(), "smtp.password is required") {
		t.Errorf("error = %q, want substring 'smtp.password is required'", err.Error())
	}
}

func TestCheckpointPathDerivedFromPipeName(t *testing.T) {
	cfg, err := Load(validArgs())
	if err != nil {
		t.Fatal(err)
	}
	// Default pipe name is "EmailTransport".
	want := DeriveCheckpointPath("EmailTransport")
	if cfg.Checkpoint.Path != want {
		t.Errorf("checkpoint.path = %q, want %q", cfg.Checkpoint.Path, want)
	}
	if !strings.HasPrefix(cfg.Checkpoint.Path, "./checkpoint-EmailTransport-") {
		t.Errorf("checkpoint.path = %q, expected prefix ./checkpoint-EmailTransport-", cfg.Checkpoint.Path)
	}
}

func TestCheckpointPathExplicitOverride(t *testing.T) {
	args := append(validArgs(), "--checkpoint-path", "/tmp/my-cp.json")
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Checkpoint.Path != "/tmp/my-cp.json" {
		t.Errorf("checkpoint.path = %q, want /tmp/my-cp.json", cfg.Checkpoint.Path)
	}
}

func TestCheckpointPathSanitization(t *testing.T) {
	tests := []struct {
		name     string
		pipeName string
	}{
		{"slash", "foo/bar"},
		{"backslash", "foo\\bar"},
		{"colon", "foo:bar"},
		{"mixed unsafe", "a/b:c\\d"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := DeriveCheckpointPath(tt.pipeName)
			// Must not contain path-unsafe characters (aside from the ./ prefix).
			after := strings.TrimPrefix(path, "./")
			for _, c := range []string{"/", "\\", ":"} {
				if strings.Contains(after, c) {
					t.Errorf("DeriveCheckpointPath(%q) = %q contains %q", tt.pipeName, path, c)
				}
			}
		})
	}
}

func TestCheckpointPathDistinctForDistinctNames(t *testing.T) {
	// Names that sanitize to the same string but differ in the original.
	p1 := DeriveCheckpointPath("foo/bar")
	p2 := DeriveCheckpointPath("foo:bar")
	if p1 == p2 {
		t.Errorf("distinct pipe names produced same path: %q", p1)
	}
}

func TestValidation_MTUExceedsMaxPacketSize(t *testing.T) {
	cfg := validConfig()
	cfg.Pipe.MTU = 766267 // MaxPacketSize + 1
	if err := Validate(&cfg); err == nil {
		t.Error("expected validation error for MTU > MaxPacketSize, got nil")
	}
}

func TestValidation_MTUAtMaxPacketSize(t *testing.T) {
	cfg := validConfig()
	cfg.Pipe.MTU = 766266 // MaxPacketSize
	if err := Validate(&cfg); err != nil {
		t.Errorf("MTU = MaxPacketSize should pass validation, got: %v", err)
	}
}

func TestCheckpointPathCustomPipeName(t *testing.T) {
	args := append(validArgs(), "--pipe-name", "MyCustomPipe")
	cfg, err := Load(args)
	if err != nil {
		t.Fatal(err)
	}
	want := DeriveCheckpointPath("MyCustomPipe")
	if cfg.Checkpoint.Path != want {
		t.Errorf("checkpoint.path = %q, want %q", cfg.Checkpoint.Path, want)
	}
}
