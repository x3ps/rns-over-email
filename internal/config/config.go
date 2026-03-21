package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/mail"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all configuration for the email transport.
type Config struct {
	Pipe       PipeConfig
	SMTP       SMTPConfig
	IMAP       IMAPConfig
	Checkpoint CheckpointConfig
	Peer       PeerConfig
	Logging    LoggingConfig
}

type PipeConfig struct {
	Name string
	MTU  int
}

type SMTPConfig struct {
	Host             string
	Port             int
	Username         string
	Password         string
	From             string
	TLS              string
	RecoveryDelay    int // base recovery backoff in seconds
	MaxRecoveryDelay int // max recovery backoff in seconds
}

type IMAPConfig struct {
	Host              string
	Port              int
	Username          string
	Password          string
	Folder            string
	TLS               string
	PollInterval      Duration
	ReconnectDelay    int // base reconnect backoff in seconds
	MaxReconnectDelay int // max reconnect backoff in seconds
	Cleanup           CleanupConfig
}

type CleanupConfig struct {
	Mode         string // "none" (default), "delete", "move"
	TargetFolder string // required when mode = "move"
}

type CheckpointConfig struct {
	Path string
}

type PeerConfig struct {
	Email string
}

type LoggingConfig struct {
	Level  string
	Format string
}

// Duration wraps time.Duration for text unmarshaling.
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalText(text []byte) error {
	var err error
	d.Duration, err = time.ParseDuration(string(text))
	return err
}

func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.Duration.String()), nil
}

// Defaults returns a Config populated with default values.
func Defaults() Config {
	return Config{
		Pipe: PipeConfig{
			Name: "EmailTransport",
			MTU:  500,
		},
		SMTP: SMTPConfig{
			Port:             587,
			TLS:              "starttls",
			RecoveryDelay:    300,
			MaxRecoveryDelay: 1800,
		},
		IMAP: IMAPConfig{
			Port:              993,
			Folder:            "INBOX",
			TLS:               "tls",
			PollInterval:      Duration{60 * time.Second},
			ReconnectDelay:    5,
			MaxReconnectDelay: 300,
			Cleanup:           CleanupConfig{Mode: "none"},
		},
		Checkpoint: CheckpointConfig{
			Path: "./checkpoint.json",
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

// Load resolves config from flags and env with proper precedence:
// Defaults -> env -> flags -> password-files -> Normalize -> Validate.
func Load(args []string) (*Config, error) {
	cfg := Defaults()

	fs := flag.NewFlagSet("rns-over-email", flag.ContinueOnError)

	pipeName := fs.String("pipe-name", "", "pipe interface name")
	pipeMTU := fs.Int("pipe-mtu", 0, "pipe MTU")
	smtpHost := fs.String("smtp-host", "", "SMTP server host")
	smtpPort := fs.Int("smtp-port", 0, "SMTP server port")
	smtpUsername := fs.String("smtp-username", "", "SMTP username")
	smtpPassword := fs.String("smtp-password", "", "SMTP password (visible in process listing; prefer --smtp-password-file)")
	smtpPasswordFile := fs.String("smtp-password-file", "", "path to file containing SMTP password (first line)")
	smtpFrom := fs.String("smtp-from", "", "SMTP envelope from address")
	smtpTLS := fs.String("smtp-tls", "", "SMTP TLS mode (tls, starttls, none)")
	smtpRecoveryDelay := fs.Int("smtp-recovery-delay", 0, "SMTP base recovery backoff after send failure, in seconds")
	smtpMaxRecoveryDelay := fs.Int("smtp-max-recovery-delay", 0, "SMTP max recovery backoff, in seconds")
	imapHost := fs.String("imap-host", "", "IMAP server host")
	imapPort := fs.Int("imap-port", 0, "IMAP server port")
	imapUsername := fs.String("imap-username", "", "IMAP username")
	imapPassword := fs.String("imap-password", "", "IMAP password (visible in process listing; prefer --imap-password-file)")
	imapPasswordFile := fs.String("imap-password-file", "", "path to file containing IMAP password (first line)")
	imapFolder := fs.String("imap-folder", "", "IMAP folder to watch")
	imapTLS := fs.String("imap-tls", "", "IMAP TLS mode (tls, starttls, none)")
	imapPollInterval := fs.String("imap-poll-interval", "", "IMAP poll interval (e.g. 60s)")
	imapReconnectDelay := fs.Int("imap-reconnect-delay", 0, "IMAP base reconnect backoff, in seconds")
	imapMaxReconnectDelay := fs.Int("imap-max-reconnect-delay", 0, "IMAP max reconnect backoff, in seconds")
	imapCleanupMode := fs.String("imap-cleanup-mode", "", "IMAP cleanup mode (none, delete, move)")
	imapCleanupTargetFolder := fs.String("imap-cleanup-target-folder", "", "IMAP cleanup target folder (for move mode)")
	peerEmail := fs.String("peer-email", "", "remote peer email address")
	checkpointPath := fs.String("checkpoint-path", "", "checkpoint file path")
	logLevel := fs.String("log-level", "", "log level (debug, info, warn, error)")
	logFormat := fs.String("log-format", "", "log format (text, json)")

	if err := fs.Parse(args); err != nil {
		return nil, fmt.Errorf("parsing flags: %w", err)
	}

	// Apply env overrides (env > defaults).
	if err := applyEnv(&cfg); err != nil {
		return nil, err
	}

	// Apply flag overrides (flags > env > defaults).
	if *pipeName != "" {
		cfg.Pipe.Name = *pipeName
	}
	if *pipeMTU != 0 {
		cfg.Pipe.MTU = *pipeMTU
	}
	if *smtpHost != "" {
		cfg.SMTP.Host = *smtpHost
	}
	if *smtpPort != 0 {
		cfg.SMTP.Port = *smtpPort
	}
	if *smtpUsername != "" {
		cfg.SMTP.Username = *smtpUsername
	}
	if *smtpPassword != "" {
		cfg.SMTP.Password = *smtpPassword
	}
	if *smtpFrom != "" {
		cfg.SMTP.From = *smtpFrom
	}
	if *smtpTLS != "" {
		cfg.SMTP.TLS = *smtpTLS
	}
	if *smtpRecoveryDelay != 0 {
		cfg.SMTP.RecoveryDelay = *smtpRecoveryDelay
	}
	if *smtpMaxRecoveryDelay != 0 {
		cfg.SMTP.MaxRecoveryDelay = *smtpMaxRecoveryDelay
	}
	if *imapHost != "" {
		cfg.IMAP.Host = *imapHost
	}
	if *imapPort != 0 {
		cfg.IMAP.Port = *imapPort
	}
	if *imapUsername != "" {
		cfg.IMAP.Username = *imapUsername
	}
	if *imapPassword != "" {
		cfg.IMAP.Password = *imapPassword
	}
	if *imapFolder != "" {
		cfg.IMAP.Folder = *imapFolder
	}
	if *imapTLS != "" {
		cfg.IMAP.TLS = *imapTLS
	}
	if *imapPollInterval != "" {
		d, err := time.ParseDuration(*imapPollInterval)
		if err != nil {
			return nil, fmt.Errorf("invalid --imap-poll-interval: %w", err)
		}
		cfg.IMAP.PollInterval = Duration{d}
	}
	if *imapReconnectDelay != 0 {
		cfg.IMAP.ReconnectDelay = *imapReconnectDelay
	}
	if *imapMaxReconnectDelay != 0 {
		cfg.IMAP.MaxReconnectDelay = *imapMaxReconnectDelay
	}
	if *imapCleanupMode != "" {
		cfg.IMAP.Cleanup.Mode = *imapCleanupMode
	}
	if *imapCleanupTargetFolder != "" {
		cfg.IMAP.Cleanup.TargetFolder = *imapCleanupTargetFolder
	}
	if *peerEmail != "" {
		cfg.Peer.Email = *peerEmail
	}
	if *checkpointPath != "" {
		cfg.Checkpoint.Path = *checkpointPath
	}
	if *logLevel != "" {
		cfg.Logging.Level = *logLevel
	}
	if *logFormat != "" {
		cfg.Logging.Format = *logFormat
	}

	// Apply password files (overrides flags, env, and defaults).
	// CLI flags take priority over env; env fills in when flag is absent.
	smtpPWFile := *smtpPasswordFile
	if smtpPWFile == "" {
		smtpPWFile = os.Getenv("RNS_EMAIL_SMTP_PASSWORD_FILE")
	}
	imapPWFile := *imapPasswordFile
	if imapPWFile == "" {
		imapPWFile = os.Getenv("RNS_EMAIL_IMAP_PASSWORD_FILE")
	}
	if err := applyPasswordFiles(&cfg, smtpPWFile, imapPWFile); err != nil {
		return nil, err
	}

	Normalize(&cfg)

	if err := Validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func applyEnv(cfg *Config) error {
	if v := os.Getenv("RNS_EMAIL_PIPE_NAME"); v != "" {
		cfg.Pipe.Name = v
	}
	if v := os.Getenv("RNS_EMAIL_PIPE_MTU"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid RNS_EMAIL_PIPE_MTU=%q: %w", v, err)
		}
		cfg.Pipe.MTU = n
	}
	if v := os.Getenv("RNS_EMAIL_SMTP_HOST"); v != "" {
		cfg.SMTP.Host = v
	}
	if v := os.Getenv("RNS_EMAIL_SMTP_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid RNS_EMAIL_SMTP_PORT=%q: %w", v, err)
		}
		cfg.SMTP.Port = n
	}
	if v := os.Getenv("RNS_EMAIL_SMTP_USERNAME"); v != "" {
		cfg.SMTP.Username = v
	}
	if v := os.Getenv("RNS_EMAIL_SMTP_PASSWORD"); v != "" {
		cfg.SMTP.Password = v
	}
	if v := os.Getenv("RNS_EMAIL_SMTP_FROM"); v != "" {
		cfg.SMTP.From = v
	}
	if v := os.Getenv("RNS_EMAIL_SMTP_TLS"); v != "" {
		cfg.SMTP.TLS = v
	}
	if v := os.Getenv("RNS_EMAIL_SMTP_RECOVERY_DELAY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid RNS_EMAIL_SMTP_RECOVERY_DELAY=%q: must be integer seconds: %w", v, err)
		}
		cfg.SMTP.RecoveryDelay = n
	}
	if v := os.Getenv("RNS_EMAIL_SMTP_MAX_RECOVERY_DELAY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid RNS_EMAIL_SMTP_MAX_RECOVERY_DELAY=%q: must be integer seconds: %w", v, err)
		}
		cfg.SMTP.MaxRecoveryDelay = n
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_HOST"); v != "" {
		cfg.IMAP.Host = v
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid RNS_EMAIL_IMAP_PORT=%q: %w", v, err)
		}
		cfg.IMAP.Port = n
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_USERNAME"); v != "" {
		cfg.IMAP.Username = v
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_PASSWORD"); v != "" {
		cfg.IMAP.Password = v
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_FOLDER"); v != "" {
		cfg.IMAP.Folder = v
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_TLS"); v != "" {
		cfg.IMAP.TLS = v
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("invalid RNS_EMAIL_IMAP_POLL_INTERVAL=%q: %w", v, err)
		}
		cfg.IMAP.PollInterval = Duration{d}
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_RECONNECT_DELAY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid RNS_EMAIL_IMAP_RECONNECT_DELAY=%q: must be integer seconds: %w", v, err)
		}
		cfg.IMAP.ReconnectDelay = n
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_MAX_RECONNECT_DELAY"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid RNS_EMAIL_IMAP_MAX_RECONNECT_DELAY=%q: must be integer seconds: %w", v, err)
		}
		cfg.IMAP.MaxReconnectDelay = n
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_CLEANUP_MODE"); v != "" {
		cfg.IMAP.Cleanup.Mode = v
	}
	if v := os.Getenv("RNS_EMAIL_IMAP_CLEANUP_TARGET_FOLDER"); v != "" {
		cfg.IMAP.Cleanup.TargetFolder = v
	}
	if v := os.Getenv("RNS_EMAIL_CHECKPOINT_PATH"); v != "" {
		cfg.Checkpoint.Path = v
	}
	if v := os.Getenv("RNS_EMAIL_PEER_EMAIL"); v != "" {
		cfg.Peer.Email = v
	}
	if v := os.Getenv("RNS_EMAIL_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
	if v := os.Getenv("RNS_EMAIL_LOG_FORMAT"); v != "" {
		cfg.Logging.Format = v
	}
	return nil
}

func applyPasswordFiles(cfg *Config, smtpPWFile, imapPWFile string) error {
	if smtpPWFile != "" {
		pw, err := readPasswordFile(smtpPWFile)
		if err != nil {
			return fmt.Errorf("reading --smtp-password-file: %w", err)
		}
		cfg.SMTP.Password = pw
	}
	if imapPWFile != "" {
		pw, err := readPasswordFile(imapPWFile)
		if err != nil {
			return fmt.Errorf("reading --imap-password-file: %w", err)
		}
		cfg.IMAP.Password = pw
	}
	return nil
}

func readPasswordFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	line := strings.SplitN(string(data), "\n", 2)[0]
	return strings.TrimRight(line, "\r"), nil
}

// Normalize applies case and whitespace normalization to enum-like fields.
// Called after all config sources are merged, before Validate.
func Normalize(cfg *Config) {
	cfg.SMTP.Host = strings.ToLower(strings.TrimSpace(cfg.SMTP.Host))
	cfg.IMAP.Host = strings.ToLower(strings.TrimSpace(cfg.IMAP.Host))
	cfg.SMTP.TLS = strings.ToLower(strings.TrimSpace(cfg.SMTP.TLS))
	cfg.IMAP.TLS = strings.ToLower(strings.TrimSpace(cfg.IMAP.TLS))
	cfg.IMAP.Cleanup.Mode = strings.ToLower(strings.TrimSpace(cfg.IMAP.Cleanup.Mode))
	cfg.Logging.Level = strings.ToLower(strings.TrimSpace(cfg.Logging.Level))
	cfg.Logging.Format = strings.ToLower(strings.TrimSpace(cfg.Logging.Format))

	// Strip display names from email addresses so SMTP MAIL FROM / RCPT TO
	// receive bare addresses (e.g. "User <u@x.com>" → "u@x.com").
	if cfg.SMTP.From != "" {
		if parsed, err := mail.ParseAddress(cfg.SMTP.From); err == nil {
			cfg.SMTP.From = parsed.Address
		}
	}
	if cfg.Peer.Email != "" {
		if parsed, err := mail.ParseAddress(cfg.Peer.Email); err == nil {
			cfg.Peer.Email = parsed.Address
		}
	}
}

var validTLS = map[string]bool{"tls": true, "starttls": true, "none": true}

// containsUnsafe returns true if s contains CR, LF, or null bytes
// that could enable SMTP header/command injection.
func containsUnsafe(s string) bool {
	return strings.ContainsAny(s, "\r\n\x00")
}

// Validate checks that the config is internally consistent.
// Values are assumed to be normalized (call Normalize first).
func Validate(cfg *Config) error {
	var errs []string

	if cfg.Pipe.Name == "" {
		errs = append(errs, "pipe.name is required")
	}
	if cfg.Pipe.MTU <= 0 {
		errs = append(errs, "pipe.mtu must be positive")
	}
	if cfg.SMTP.Port <= 0 || cfg.SMTP.Port > 65535 {
		errs = append(errs, "smtp.port must be 1-65535")
	}
	if cfg.SMTP.Host == "" {
		errs = append(errs, "smtp.host is required")
	}
	if cfg.SMTP.Username == "" {
		errs = append(errs, "smtp.username is required")
	}
	if cfg.SMTP.Password == "" {
		errs = append(errs, "smtp.password is required")
	}
	if cfg.SMTP.From == "" {
		errs = append(errs, "smtp.from is required")
	}
	if !validTLS[cfg.SMTP.TLS] {
		errs = append(errs, fmt.Sprintf("smtp.tls %q is not valid (tls, starttls, none)", cfg.SMTP.TLS))
	}
	if cfg.IMAP.Host == "" {
		errs = append(errs, "imap.host is required")
	}
	if cfg.IMAP.Port <= 0 || cfg.IMAP.Port > 65535 {
		errs = append(errs, "imap.port must be 1-65535")
	}
	if cfg.IMAP.Username == "" {
		errs = append(errs, "imap.username is required")
	}
	if cfg.IMAP.Password == "" {
		errs = append(errs, "imap.password is required")
	}
	if cfg.IMAP.Folder == "" {
		errs = append(errs, "imap.folder is required")
	}
	if !validTLS[cfg.IMAP.TLS] {
		errs = append(errs, fmt.Sprintf("imap.tls %q is not valid (tls, starttls, none)", cfg.IMAP.TLS))
	}
	if cfg.Checkpoint.Path == "" {
		errs = append(errs, "checkpoint.path is required")
	}
	if cfg.IMAP.PollInterval.Duration <= 0 {
		errs = append(errs, "imap.poll_interval must be positive")
	}
	if cfg.IMAP.ReconnectDelay <= 0 {
		errs = append(errs, "imap.reconnect_delay must be positive")
	}
	if cfg.IMAP.MaxReconnectDelay <= 0 {
		errs = append(errs, "imap.max_reconnect_delay must be positive")
	}
	if cfg.IMAP.MaxReconnectDelay < cfg.IMAP.ReconnectDelay {
		errs = append(errs, "imap.max_reconnect_delay must be >= imap.reconnect_delay")
	}
	if cfg.SMTP.RecoveryDelay <= 0 {
		errs = append(errs, "smtp.recovery_delay must be positive")
	}
	if cfg.SMTP.MaxRecoveryDelay <= 0 {
		errs = append(errs, "smtp.max_recovery_delay must be positive")
	}
	if cfg.SMTP.MaxRecoveryDelay < cfg.SMTP.RecoveryDelay {
		errs = append(errs, "smtp.max_recovery_delay must be >= smtp.recovery_delay")
	}
	validCleanup := map[string]bool{"none": true, "delete": true, "move": true}
	if !validCleanup[cfg.IMAP.Cleanup.Mode] {
		errs = append(errs, fmt.Sprintf("imap.cleanup.mode %q is not valid (none, delete, move)", cfg.IMAP.Cleanup.Mode))
	}
	if cfg.IMAP.Cleanup.Mode == "move" && cfg.IMAP.Cleanup.TargetFolder == "" {
		errs = append(errs, "imap.cleanup.target_folder is required when cleanup mode is \"move\"")
	}
	if cfg.IMAP.Cleanup.Mode == "move" && cfg.IMAP.Cleanup.TargetFolder == cfg.IMAP.Folder {
		errs = append(errs, "imap.cleanup.target_folder must differ from imap.folder")
	}

	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[cfg.Logging.Level] {
		errs = append(errs, fmt.Sprintf("logging.level %q is not valid (debug, info, warn, error)", cfg.Logging.Level))
	}
	validFormats := map[string]bool{"text": true, "json": true}
	if !validFormats[cfg.Logging.Format] {
		errs = append(errs, fmt.Sprintf("logging.format %q is not valid (text, json)", cfg.Logging.Format))
	}

	if cfg.Peer.Email == "" {
		errs = append(errs, "peer.email is required")
	} else if _, err := mail.ParseAddress(cfg.Peer.Email); err != nil {
		errs = append(errs, fmt.Sprintf("peer.email %q is not a valid address: %v", cfg.Peer.Email, err))
	}
	if cfg.SMTP.From != "" {
		if _, err := mail.ParseAddress(cfg.SMTP.From); err != nil {
			errs = append(errs, fmt.Sprintf("smtp.from %q is not a valid address: %v", cfg.SMTP.From, err))
		}
	}
	if containsUnsafe(cfg.SMTP.From) {
		errs = append(errs, "smtp.from contains invalid control characters")
	}
	if containsUnsafe(cfg.Peer.Email) {
		errs = append(errs, "peer.email contains invalid control characters")
	}

	if len(errs) > 0 {
		return errors.New("config validation: " + strings.Join(errs, "; "))
	}
	return nil
}

// WarnInsecureTLS logs warnings if TLS is disabled for SMTP or IMAP (RFC 8314).
func WarnInsecureTLS(cfg *Config, logger *slog.Logger) {
	if cfg.SMTP.TLS == "none" {
		logger.Warn("smtp.tls=none: credentials sent in plaintext (RFC 8314 recommends TLS)")
	}
	if cfg.IMAP.TLS == "none" {
		logger.Warn("imap.tls=none: credentials sent in plaintext (RFC 8314 recommends TLS)")
	}
}

// SlogLevel converts the string log level to slog.Level.
func (cfg *Config) SlogLevel() slog.Level {
	switch cfg.Logging.Level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
