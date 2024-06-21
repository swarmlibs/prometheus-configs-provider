package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"os"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/oklog/run"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

var (
	version = "dev"
)

func main() {
	app := kingpin.New("dockerswarm-configs-provider", "")

	outputDir := app.Flag("output-dir", "directory for the configs").Default("out").String()
	outputExt := app.Flag("output-ext", "extension for the configs").Default("yaml").String()

	var logger log.Logger
	logger = log.NewLogfmtLogger(os.Stdout)
	logger = level.NewFilter(logger, level.AllowAll())
	logger = log.With(logger, "ts", log.DefaultTimestampUTC)
	logger = log.With(logger, "caller", log.DefaultCaller)

	if _, err := app.Parse(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stdout, err)
		os.Exit(2)
	}

	level.Info(logger).Log("msg", "Starting dockerswarm-configs-provider", "version", version)

	var (
		g           run.Group
		ctx, cancel = context.WithCancel(context.Background())
	)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}

	// On startup, remove all existing files in the output directory
	{
		level.Info(logger).Log("msg", "Cleaning up existing files in output directory")
		files, _ := os.ReadDir(*outputDir)
		for _, file := range files {
			if file.IsDir() {
				continue
			}
			if err := os.Remove(fmt.Sprintf("%s/%s", *outputDir, file.Name())); err != nil {
				level.Error(logger).Log("msg", "Failed to remove file", "file", file.Name(), "err", err)
			}
		}
	}

	{
		level.Info(logger).Log("msg", "Generating files from existing list of configs")
		configs, err := cli.ConfigList(ctx, types.ConfigListOptions{})
		if err != nil {
			panic(err)
		}
		for _, config := range configs {
			cfg, _, err := cli.ConfigInspectWithRaw(ctx, config.ID)
			if err != nil {
				continue
			}
			outFile := fmt.Sprintf("%s/%s.%s", *outputDir, cfg.ID, *outputExt)
			level.Info(logger).Log("msg", "Event triggered", "type", "read", "id", config.ID, "file", outFile)
			writeConfigToFile(outFile, cfg.Spec.Data)
		}
	}

	// Subscribe to Docker events for configs
	level.Info(logger).Log("msg", "Subscribing to Docker events")

	filters := filters.NewArgs()
	filters.Add("type", "config")
	events, errCh := cli.Events(ctx, events.ListOptions{
		Filters: filters,
	})
	g.Add(func() error {
		for {
			select {
			case event := <-events:
				switch event.Action {
				case "create", "update":
					cfg, _, err := cli.ConfigInspectWithRaw(ctx, event.Actor.ID)

					if err != nil {
						level.Error(logger).Log("msg", "Failed to read config", "id", event.Actor.ID, "err", err)
						continue
					}

					if cfg.Spec.Labels["io.prometheus.scrape_config"] == "" {
						continue
					}

					outFile := fmt.Sprintf("%s/%s.%s", *outputDir, cfg.ID, *outputExt)
					level.Info(logger).Log("msg", "Event triggered", "type", event.Type, "action", event.Action, "id", event.Actor.ID, "file", outFile)

					writeConfigToFile(outFile, cfg.Spec.Data)
				case "remove":
					outFile := fmt.Sprintf("%s/%s.%s", *outputDir, event.Actor.ID, *outputExt)

					if _, err := os.Stat(outFile); err == nil {
						level.Info(logger).Log("msg", "Event triggered", "type", event.Type, "action", event.Action, "id", event.Actor.ID, "file", outFile)
						if err := os.Remove(outFile); err != nil {
							level.Error(logger).Log("msg", "Failed to remove file", "id", event.Actor.ID, "file", outFile, "err", err)
						}
					}
				}
			case err := <-errCh:
				level.Error(logger).Log("msg", "Failed to receive Docker events", "err", err)
				return err
			}
		}
	}, func(error) {
		cli.Close()
		cancel()
	})

	term := make(chan os.Signal, 1)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)
	g.Add(func() error {
		select {
		case <-term:
			level.Info(logger).Log("msg", "Received SIGTERM, exiting gracefully...")
		case <-ctx.Done():
		}

		return nil
	}, func(error) {})

	if err := g.Run(); err != nil {
		level.Error(logger).Log("msg", "Failed to run", "err", err)
		os.Exit(1)
	}
}

func writeConfigToFile(filename string, data []byte) {
	file, _ := os.Create(filename)
	defer file.Close()
	file.WriteString(string(data))
}
