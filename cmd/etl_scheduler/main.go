package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/Aturjadwal/singapay-ledger/analytics"
)

type config struct {
	databaseURL          string
	databaseLedgerURL    string
	databaseAnalyticsURL string
	dbMaxConns           int
	dbMaxIdleConns       int
	redisHost            string
	redisUsername        string
	redisPassword        string
	redisCluster         bool
	redisDB              int
	interval             time.Duration
	once                 bool
	mode                 string
	endTime              *time.Time
	recalculateDate      *time.Time
	recalculateEnd       *time.Time
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("etl scheduler booting", "args_count", len(args))

	cfg, err := parseConfig(args)
	if err != nil {
		logger.Error("failed to parse config", "error", err)
		return err
	}

	logger.Info("config parsed",
		"mode", cfg.mode,
		"once", cfg.once,
		"interval", cfg.interval,
		"redis_host", cfg.redisHost,
		"redis_cluster", cfg.redisCluster,
		"redis_db", cfg.redisDB,
		"has_end_time", cfg.endTime != nil,
		"has_recalculate_date", cfg.recalculateDate != nil,
		"has_recalculate_end_date", cfg.recalculateEnd != nil,
	)

	// Determine ledger/analytics DB URLs: fall back to single DATABASE_URL if specific ones not provided
	ledgerURL := cfg.databaseLedgerURL
	analyticsURL := cfg.databaseAnalyticsURL

	if ledgerURL == "" {
		ledgerURL = cfg.databaseURL
	}
	if analyticsURL == "" {
		analyticsURL = cfg.databaseURL
	}

	ledgerDB, err := sql.Open("postgres", ledgerURL)
	if err != nil {
		logger.Error("failed to open ledger (source) database", "error", err)
		return fmt.Errorf("open ledger database: %w", err)
	}
	defer ledgerDB.Close()

	ledgerAnalyticsDB, err := sql.Open("postgres", analyticsURL)
	if err != nil {
		logger.Error("failed to open analytics (target) database", "error", err)
		return fmt.Errorf("open analytics database: %w", err)
	}
	defer ledgerAnalyticsDB.Close()

	// Use same pool settings for both DBs
	for _, dbConn := range []*sql.DB{ledgerDB, ledgerAnalyticsDB} {
		dbConn.SetMaxOpenConns(cfg.dbMaxConns)
		dbConn.SetMaxIdleConns(cfg.dbMaxIdleConns)
		dbConn.SetConnMaxLifetime(30 * time.Minute)
		if err := dbConn.Ping(); err != nil {
			logger.Error("failed to ping database", "error", err)
			return fmt.Errorf("ping database: %w", err)
		}
	}
	logger.Info("databases connected", "ledger_db", ledgerURL != "", "ledger_analytics_db", analyticsURL != "")

	var redisClient redis.UniversalClient
	if cfg.redisCluster {
		redisClient = redis.NewClusterClient(&redis.ClusterOptions{
			Addrs:    splitAndTrim(cfg.redisHost),
			Username: cfg.redisUsername,
			Password: cfg.redisPassword,
		})
	} else {
		redisClient = redis.NewClient(&redis.Options{
			Addr:     cfg.redisHost,
			Username: cfg.redisUsername,
			Password: cfg.redisPassword,
			DB:       cfg.redisDB,
		})
	}
	defer redisClient.Close()

	if err := redisClient.Ping(context.Background()).Err(); err != nil {
		logger.Error("failed to ping redis", "error", err)
		return fmt.Errorf("ping redis: %w", err)
	}
	logger.Info("redis connected")

	// No gateway credentials here any more: the analytics client is pure SQL. dim_bank
	// used to be filled from the gateway's supported-bank catalogue; Singapay has none,
	// so it is derived from observed disbursements instead.
	client := analytics.NewLedgerAnalyticsClient(ledgerDB, ledgerAnalyticsDB, redisClient, logger)
	opts := analytics.ETLOptions{
		EndTime:            cfg.endTime,
		RecalculateDate:    cfg.recalculateDate,
		RecalculateEndDate: cfg.recalculateEnd,
	}
	logger.Info("analytics client initialized")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.once {
		opts.RunID = newRunID()
		logger.Info("running single etl cycle", "mode", cfg.mode, "run_id", opts.RunID)
		if err := runSelectedJob(ctx, client, cfg.mode, opts, logger); err != nil {
			logger.Error("single etl cycle failed", "mode", cfg.mode, "run_id", opts.RunID, "error", err)
			return err
		}
		logger.Info("single etl cycle completed", "mode", cfg.mode, "run_id", opts.RunID)
		return nil
	}

	logger.Info("starting scheduled etl execution", "mode", cfg.mode, "interval", cfg.interval)
	return runScheduler(ctx, client, cfg.mode, opts, cfg.interval, logger)
}

func parseConfig(args []string) (*config, error) {
	fs := flag.NewFlagSet("etl_scheduler", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  etl_scheduler [flags]")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Flags:")
		fs.PrintDefaults()
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Modes:")
		fmt.Fprintln(fs.Output(), "  full, dimensions, facts")
		fmt.Fprintln(fs.Output(), "  static, account, bank-account, payment-channel")
		fmt.Fprintln(fs.Output(), "  fact-revenue, fact-platform-balance, fact-user-accumulation, fact-withdrawal")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Environment Variables:")
		fmt.Fprintln(fs.Output(), "  DATABASE_URL (preferred)")
		fmt.Fprintln(fs.Output(), "  DATABASE_LEDGER_URL, DATABASE_ANALYTICS_URL (or DATABASE_URL)")
		fmt.Fprintln(fs.Output(), "  DB_MAXCONNS, DB_MAXIDLECONNS")
		fmt.Fprintln(fs.Output(), "  REDIS_HOST, REDIS_USER, REDIS_PASS, REDIS_CLUSTER, REDIS_DB")
		fmt.Fprintln(fs.Output(), "  ETL_MODE, ETL_INTERVAL, ETL_ONCE, ETL_END_TIME, ETL_RECALCULATE_DATE, ETL_RECALCULATE_END_DATE")
		fmt.Fprintln(fs.Output(), "")
		fmt.Fprintln(fs.Output(), "Examples:")
		fmt.Fprintln(fs.Output(), "  etl_scheduler --once --mode full")
		fmt.Fprintln(fs.Output(), "  etl_scheduler --mode full --interval 5m")
		fmt.Fprintln(fs.Output(), "  etl_scheduler --once --mode full --recalculate-date 2026-04-11")
		fmt.Fprintln(fs.Output(), "  etl_scheduler --once --mode full --recalculate-date 2026-01-01 --recalculate-end-date 2026-01-31")
		fmt.Fprintln(fs.Output(), "  etl_scheduler --once --mode static --recalculate-date 2026-04-11")
	}

	cfg := &config{}
	fs.StringVar(&cfg.databaseURL, "database-url", getenv("DATABASE_URL", ""), "PostgreSQL connection string (takes precedence over DB_* envs)")
	fs.StringVar(&cfg.databaseLedgerURL, "database-ledger-url", getenv("DATABASE_LEDGER_URL", ""), "PostgreSQL ledger/source DB connection string (optional)")
	fs.StringVar(&cfg.databaseAnalyticsURL, "database-analytics-url", getenv("DATABASE_ANALYTICS_URL", ""), "PostgreSQL analytics/target DB connection string (optional)")
	fs.IntVar(&cfg.dbMaxConns, "db-max-conns", getenvInt("DB_MAXCONNS", 30), "PostgreSQL max open connections")
	fs.IntVar(&cfg.dbMaxIdleConns, "db-max-idle-conns", getenvInt("DB_MAXIDLECONNS", 10), "PostgreSQL max idle connections")
	fs.StringVar(&cfg.redisHost, "redis-host", getenv("REDIS_HOST", "localhost:6379"), "Redis host/address (comma-separated when REDIS_CLUSTER=true)")
	fs.StringVar(&cfg.redisUsername, "redis-user", getenv("REDIS_USER", ""), "Redis username")
	fs.StringVar(&cfg.redisPassword, "redis-pass", getenv("REDIS_PASS", ""), "Redis password")
	fs.BoolVar(&cfg.redisCluster, "redis-cluster", getenvBool("REDIS_CLUSTER", false), "Use Redis cluster mode")
	fs.IntVar(&cfg.redisDB, "redis-db", getenvInt("REDIS_DB", 0), "Redis database index (non-cluster mode)")
	fs.DurationVar(&cfg.interval, "interval", getenvDuration("ETL_INTERVAL", 30*time.Minute), "Interval between runs")
	fs.BoolVar(&cfg.once, "once", getenvBool("ETL_ONCE", false), "Run a single ETL cycle and exit")
	fs.StringVar(&cfg.mode, "mode", getenv("ETL_MODE", "full"), "ETL mode: full, dimensions, facts, static, account, bank-account, payment-channel, fact-revenue, fact-platform-balance, fact-user-accumulation, fact-withdrawal")

	endTimeFlag := fs.String("end-time", getenv("ETL_END_TIME", ""), "Optional ETL end time in RFC3339")
	recalculateDateFlag := fs.String("recalculate-date", getenv("ETL_RECALCULATE_DATE", ""), "Optional date to force ETL watermark for all jobs and recalculate dim_date (YYYY-MM-DD)")
	recalculateEndFlag := fs.String("recalculate-end-date", getenv("ETL_RECALCULATE_END_DATE", ""), "Optional end date for recalculation range (YYYY-MM-DD)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	// Require either DATABASE_URL or both per-DB URLs
	if cfg.databaseURL == "" && (cfg.databaseLedgerURL == "" || cfg.databaseAnalyticsURL == "") {
		return nil, errors.New("database config is required: set DATABASE_URL or both DATABASE_LEDGER_URL and DATABASE_ANALYTICS_URL")
	}
	if cfg.redisHost == "" {
		return nil, errors.New("redis-host is required (set REDIS_HOST or pass --redis-host)")
	}

	if cfg.interval <= 0 {
		cfg.interval = 30 * time.Minute
	}

	if *endTimeFlag != "" {
		parsed, err := time.Parse(time.RFC3339, *endTimeFlag)
		if err != nil {
			return nil, fmt.Errorf("parse end-time: %w", err)
		}
		parsed = parsed.UTC()
		cfg.endTime = &parsed
	}

	if *recalculateDateFlag != "" {
		parsed, err := time.Parse("2006-01-02", *recalculateDateFlag)
		if err != nil {
			return nil, fmt.Errorf("parse recalculate-date: %w", err)
		}
		dateOnly := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.UTC)
		cfg.recalculateDate = &dateOnly
	}

	if *recalculateEndFlag != "" {
		parsed, err := time.Parse("2006-01-02", *recalculateEndFlag)
		if err != nil {
			return nil, fmt.Errorf("parse recalculate-end-date: %w", err)
		}
		dateOnly := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.UTC)
		cfg.recalculateEnd = &dateOnly
	}

	if cfg.recalculateEnd != nil && cfg.recalculateDate == nil {
		return nil, errors.New("recalculate-end-date requires recalculate-date")
	}
	if cfg.recalculateDate != nil && cfg.recalculateEnd != nil && cfg.recalculateEnd.Before(*cfg.recalculateDate) {
		return nil, errors.New("recalculate-end-date must be greater than or equal to recalculate-date")
	}

	cfg.mode = strings.ToLower(strings.TrimSpace(cfg.mode))
	if cfg.mode == "" {
		cfg.mode = "full"
	}

	return cfg, nil
}

func buildPostgresURL(host, port, user, password, dbName, sslMode string) string {
	if port == "" {
		port = "5432"
	}
	if sslMode == "" {
		sslMode = "disable"
	}
	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(user, password),
		Host:   net.JoinHostPort(host, port),
		Path:   dbName,
	}
	q := u.Query()
	q.Set("sslmode", sslMode)
	u.RawQuery = q.Encode()
	return u.String()
}

func splitAndTrim(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func runScheduler(ctx context.Context, client *analytics.LedgerAnalyticsClient, mode string, opts analytics.ETLOptions, interval time.Duration, logger *slog.Logger) error {
	logger.Info("starting analytics ETL scheduler",
		"mode", mode,
		"interval", interval,
	)

	for {
		cycleOpts := opts
		cycleOpts.RunID = newRunID()
		logger.Info("starting analytics ETL cycle", "mode", mode, "run_id", cycleOpts.RunID)

		if err := runSelectedJob(ctx, client, mode, cycleOpts, logger); err != nil {
			logger.Error("analytics ETL cycle failed", "mode", mode, "run_id", cycleOpts.RunID, "error", err)
		} else {
			logger.Info("analytics ETL cycle completed", "mode", mode, "run_id", cycleOpts.RunID)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval):
		}
	}
}

func runSelectedJob(ctx context.Context, client *analytics.LedgerAnalyticsClient, mode string, opts analytics.ETLOptions, logger *slog.Logger) error {
	start := time.Now()
	logger.Info("running selected etl job", "mode", mode, "run_id", opts.RunID)

	var err error
	switch mode {
	case "full":
		err = runFullPipeline(ctx, client, opts)
	case "dimensions":
		err = runAllDimensions(ctx, client, opts)
	case "facts":
		err = runAllFacts(ctx, client, opts)
	case "static":
		err = client.RunStaticDimensionsETL(ctx, opts)
	case "account":
		err = client.RunDimAccountETL(ctx, opts)
	case "bank-account":
		err = client.RunDimBankAccountETL(ctx, opts)
	case "payment-channel":
		err = client.RunDimPaymentChannelETL(ctx, opts)
	case "fact-revenue":
		err = client.RunFactRevenueTimeseriesETL(ctx, opts)
	case "fact-platform-balance":
		err = client.RunFactPlatformBalanceETL(ctx, opts)
	case "fact-user-accumulation":
		err = client.RunFactUserAccumulationETL(ctx, opts)
	case "fact-withdrawal":
		err = client.RunFactWithdrawalTimeseriesETL(ctx, opts)
	default:
		err = fmt.Errorf("unsupported ETL mode %q", mode)
	}

	duration := time.Since(start)
	if err != nil {
		logger.Error("etl job failed", "mode", mode, "run_id", opts.RunID, "duration", duration, "error", err)
		return err
	}
	logger.Info("etl job completed", "mode", mode, "run_id", opts.RunID, "duration", duration)
	return nil
}

func newRunID() string {
	return fmt.Sprintf("run-%d", time.Now().UTC().UnixNano())
}

func runFullPipeline(ctx context.Context, client *analytics.LedgerAnalyticsClient, opts analytics.ETLOptions) error {
	if err := runAllDimensions(ctx, client, opts); err != nil {
		return err
	}
	return runAllFacts(ctx, client, opts)
}

func runAllDimensions(ctx context.Context, client *analytics.LedgerAnalyticsClient, opts analytics.ETLOptions) error {
	if err := client.RunStaticDimensionsETL(ctx, opts); err != nil {
		return err
	}
	if err := client.RunDimAccountETL(ctx, opts); err != nil {
		return err
	}
	if err := client.RunDimBankAccountETL(ctx, opts); err != nil {
		return err
	}
	return client.RunDimPaymentChannelETL(ctx, opts)
}

func runAllFacts(ctx context.Context, client *analytics.LedgerAnalyticsClient, opts analytics.ETLOptions) error {
	return client.RunAllFacts(ctx, opts)
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func getenvBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	lower := strings.ToLower(value)
	switch lower {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return fallback
	}
}

func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}
