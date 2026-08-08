package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/zanescope/vohive/internal/hostconfig"
	"github.com/zanescope/vohive/internal/updater"
)

func main() {
	global := flag.NewFlagSet("vohivectl", flag.ContinueOnError)
	global.SetOutput(os.Stderr)
	deploymentFile := global.String("deployment", updater.DefaultDeploymentPath, "deployment metadata path")
	if err := global.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	args := global.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	var err error
	switch args[0] {
	case "status":
		err = status(*deploymentFile)
	case "check":
		err = check(ctx, *deploymentFile, args[1:])
	case "update":
		err = update(ctx, *deploymentFile, args[1:])
	case "rollback":
		err = rollback(ctx, *deploymentFile, args[1:])
	case "backup":
		err = backup(ctx, *deploymentFile, args[1:])
	case "doctor":
		err = doctor(ctx, *deploymentFile, args[1:])
	case "recover":
		err = recover(ctx, *deploymentFile, args[1:])
	case "guard-start":
		err = guardStart(*deploymentFile, args[1:])
	case "host-config":
		err = applyHostConfig(ctx, args[1:])
	case "probe":
		err = probe(ctx, *deploymentFile, args[1:])
	default:
		usage()
		err = fmt.Errorf("unknown command %q", args[0])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "vohivectl: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: vohivectl [--deployment PATH] <status|check|update|rollback|backup|doctor|recover|guard-start|host-config|probe> [options]")
}

func status(deploymentFile string) error {
	deployment, err := updater.DiscoverDeployment(deploymentFile)
	if err != nil {
		return err
	}
	if err := updater.ValidateProductionScope(updater.PathsFor(deploymentFile, deployment)); err != nil {
		return err
	}
	var state *updater.TransactionState
	loaded, stateErr := updater.LoadState(updater.PathsFor(deploymentFile, deployment).StateFile())
	if stateErr == nil {
		state = &loaded
	} else if !os.IsNotExist(stateErr) {
		return stateErr
	}
	return writeJSON(struct {
		Deployment   updater.Deployment        `json:"deployment"`
		Capabilities updater.Capabilities      `json:"capabilities"`
		State        *updater.TransactionState `json:"state,omitempty"`
	}{deployment, updater.DetectCapabilities(deployment), state})
}

func check(ctx context.Context, deploymentFile string, args []string) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	channelValue := flags.String("channel", "", "stable or beta")
	version := flags.String("version", "", "exact signed release tag")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("check takes no positional arguments")
	}
	deployment, resolver, err := resolverFor(deploymentFile)
	if err != nil {
		return err
	}
	channel := deployment.Channel
	if *channelValue != "" {
		channel, err = updater.ParseChannel(*channelValue)
		if err != nil {
			return err
		}
	}
	if channel == updater.ChannelPinned && *version == "" {
		return errors.New("pinned channel requires --version")
	}
	candidate, err := resolver.Check(ctx, updater.CheckRequest{
		Channel: channel, Version: *version, CurrentVersion: deployment.CurrentVersion,
		GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
	})
	if err != nil {
		return err
	}
	return writeJSON(candidate)
}

func update(ctx context.Context, deploymentFile string, args []string) error {
	flags := flag.NewFlagSet("update", flag.ContinueOnError)
	channelValue := flags.String("channel", "", "stable, beta, or pinned")
	version := flags.String("version", "", "exact target selected during check")
	requestFile := flags.String("request", "", "read a persisted update request")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("update takes no positional arguments")
	}
	deployment, resolver, err := resolverFor(deploymentFile)
	if err != nil {
		return err
	}
	request := updater.UpdateRequest{Schema: 1, Channel: deployment.Channel, Version: *version}
	if *requestFile != "" {
		request, err = updater.LoadRequest(*requestFile)
		if err != nil {
			return err
		}
	} else if *channelValue != "" {
		request.Channel, err = updater.ParseChannel(*channelValue)
		if err != nil {
			return err
		}
	}
	if request.Channel == updater.ChannelPinned && request.Version == "" {
		return errors.New("pinned channel requires --version")
	}
	engine, err := updater.NewEngine(deploymentFile, resolver)
	if err != nil {
		return err
	}
	state, err := engine.Update(ctx, request)
	if jsonErr := writeJSON(state); err == nil && jsonErr != nil {
		err = jsonErr
	}
	return err
}

func rollback(ctx context.Context, deploymentFile string, args []string) error {
	if len(args) != 0 {
		return errors.New("rollback takes no arguments")
	}
	engine, err := updater.NewEngine(deploymentFile, nil)
	if err != nil {
		return err
	}
	state, err := engine.Rollback(ctx)
	if jsonErr := writeJSON(state); err == nil && jsonErr != nil {
		err = jsonErr
	}
	return err
}

func backup(ctx context.Context, deploymentFile string, args []string) error {
	if len(args) != 0 {
		return errors.New("backup takes no arguments")
	}
	engine, err := updater.NewEngine(deploymentFile, nil)
	if err != nil {
		return err
	}
	path, err := engine.Backup(ctx)
	if err != nil {
		return err
	}
	return writeJSON(map[string]string{"backup_path": path})
}

func doctor(ctx context.Context, deploymentFile string, args []string) error {
	if len(args) != 0 {
		return errors.New("doctor takes no arguments")
	}
	report := updater.Doctor(ctx, deploymentFile, updater.HTTPReadyChecker{Client: &http.Client{Timeout: 5 * time.Second}})
	if err := writeJSON(report); err != nil {
		return err
	}
	if !report.Healthy {
		return errors.New("one or more diagnostics failed")
	}
	return nil
}

func recover(ctx context.Context, deploymentFile string, args []string) error {
	flags := flag.NewFlagSet("recover", flag.ContinueOnError)
	start := flags.Bool("start", false, "start VoHive after recovery")
	boot := flags.Bool("boot", false, "allow boot-ordered orphan-lock recovery after confirming VoHive is inactive")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("recover takes no positional arguments")
	}
	if *start && *boot {
		return errors.New("--start and --boot cannot be combined")
	}
	engine, err := updater.NewEngine(deploymentFile, nil)
	if err != nil {
		return err
	}
	engine.BootRecovery = *boot
	state, err := engine.Recover(ctx, *start)
	if jsonErr := writeJSON(state); err == nil && jsonErr != nil {
		err = jsonErr
	}
	return err
}
func guardStart(deploymentFile string, args []string) error {
	if len(args) != 0 {
		return errors.New("guard-start takes no arguments")
	}
	return updater.GuardStart(deploymentFile)
}

func applyHostConfig(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("host-config", flag.ContinueOnError)
	requestFile := flags.String("request", "", "read a persisted, server-generated host configuration request")
	cleanupForUninstall := flags.Bool(
		"cleanup-managed-for-uninstall",
		false,
		"remove only the canonical VoHive-managed ModemManager rule during product uninstall",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("host-config takes no positional arguments")
	}
	if (*requestFile != "") == *cleanupForUninstall {
		return errors.New(
			"host-config requires exactly one of --request or --cleanup-managed-for-uninstall",
		)
	}
	if *cleanupForUninstall {
		result, cleanupErr := hostconfig.CleanupManagedForUninstall(ctx, nil)
		if jsonErr := writeJSON(result); cleanupErr == nil && jsonErr != nil {
			cleanupErr = jsonErr
		}
		return cleanupErr
	}
	if *requestFile != hostconfig.DefaultRequestPath {
		return fmt.Errorf("host-config --request must be %s", hostconfig.DefaultRequestPath)
	}
	return hostconfig.RunWorker(ctx, *requestFile, nil)
}

func probe(ctx context.Context, deploymentFile string, args []string) error {
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	configPath := flags.String("config", "", "config path (required for standalone liveness probes)")
	expectedVersion := flags.String("expected-version", "", "exact managed release version")
	liveness := flags.Bool("liveness", false, "probe process liveness without update identity")
	printURL := flags.Bool("print-url", false, "print the resolved URL after a successful probe")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("probe takes no positional arguments")
	}

	client := &http.Client{Timeout: 5 * time.Second}
	if *liveness {
		path := *configPath
		if path == "" {
			deployment, err := updater.DiscoverDeployment(deploymentFile)
			if err != nil {
				return err
			}
			path = deployment.ConfigPath
		}
		endpoint, err := updater.ResolveProbeURL(path, "/healthz")
		if err != nil {
			return err
		}
		if err := updater.ProbeHTTP(ctx, client, endpoint); err != nil {
			return err
		}
		if *printURL {
			fmt.Fprintln(os.Stdout, endpoint)
		}
		return nil
	}

	deployment, err := updater.DiscoverDeployment(deploymentFile)
	if err != nil {
		return err
	}
	paths := updater.PathsFor(deploymentFile, deployment)
	if err := updater.ValidateProductionScope(paths); err != nil {
		return err
	}
	endpoint, err := updater.ResolveReadyURL(deployment)
	if err != nil {
		return err
	}
	version := *expectedVersion
	if version == "" {
		version = deployment.CurrentVersion
	}
	expectation := updater.ReadyExpectation{
		Endpoint:        endpoint,
		ExpectedVersion: version,
		KeyFile:         paths.ReadinessKeyFile(),
	}
	if err := (updater.HTTPReadyChecker{Client: client}).Ready(ctx, expectation); err != nil {
		return err
	}
	if *printURL {
		fmt.Fprintln(os.Stdout, endpoint)
	}
	return nil
}

func resolverFor(deploymentFile string) (updater.Deployment, updater.ReleaseResolver, error) {
	deployment, err := updater.DiscoverDeployment(deploymentFile)
	if err != nil {
		return updater.Deployment{}, nil, err
	}
	if err := updater.ValidateProductionScope(updater.PathsFor(deploymentFile, deployment)); err != nil {
		return updater.Deployment{}, nil, err
	}
	verifier, err := updater.DefaultSignatureVerifier()
	if err != nil {
		return updater.Deployment{}, nil, err
	}
	resolver, err := updater.NewGitHubResolver(&http.Client{Timeout: 30 * time.Second}, verifier)
	if err != nil {
		return updater.Deployment{}, nil, err
	}
	return deployment, resolver, nil
}

func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
