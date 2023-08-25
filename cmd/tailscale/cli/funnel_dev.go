// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/fatih/color"
	"github.com/peterbourgon/ff/v3/ffcli"
	"tailscale.com/ipn"
)

// newFunnelDevCommand returns a new "funnel" subcommand using e as its environment.
// The funnel subcommand is used to turn on/off the Funnel service.
// Funnel is off by default.
// Funnel allows you to publish a 'tailscale serve' server publicly,
// open to the entire internet.
// newFunnelCommand shares the same serveEnv as the "serve" subcommand.
// See newServeCommand and serve.go for more details.
func newFunnelDevCommand(e *serveEnv) *ffcli.Command {
	return &ffcli.Command{
		Name:      "funnel",
		ShortHelp: "Turn on/off Funnel service",
		ShortUsage: strings.Join([]string{
			"funnel <port>",
			"funnel status [--json]",
		}, "\n  "),
		LongHelp: strings.Join([]string{
			"Funnel allows you to expose your local",
			"server publicly to the entire internet.",
			"Note that it only supports https servers at this point.",
			"This command is in development and is unsupported",
		}, "\n"),
		Exec:      e.runFunnelDev,
		UsageFunc: usageFunc,
		Subcommands: []*ffcli.Command{
			{
				Name:      "status",
				Exec:      e.runServeStatus,
				ShortHelp: "show current serve/Funnel status",
				FlagSet: e.newFlags("funnel-status", func(fs *flag.FlagSet) {
					fs.BoolVar(&e.json, "json", false, "output JSON")
				}),
				UsageFunc: usageFunc,
			},
		},
	}
}

// runFunnelDev is the entry point for the "tailscale funnel" subcommand and
// manages turning on/off Funnel. Funnel is off by default.
//
// Note: funnel is only supported on single DNS name for now. (2023-08-18)
func (e *serveEnv) runFunnelDev(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return flag.ErrHelp
	}
	var source string
	port64, err := strconv.ParseUint(args[0], 10, 16)
	if err == nil {
		source = fmt.Sprintf("http://127.0.0.1:%d", port64)
	} else {
		source, err = expandProxyTarget(args[0])
	}
	if err != nil {
		return err
	}

	st, err := e.getLocalClientStatusWithoutPeers(ctx)
	if err != nil {
		return fmt.Errorf("getting client status: %w", err)
	}

	if err := e.verifyFunnelEnabled(ctx, st, 443); err != nil {
		return err
	}

	dnsName := strings.TrimSuffix(st.Self.DNSName, ".")
	hp := ipn.HostPort(dnsName + ":443") // TODO(marwan-at-work): support the 2 other ports

	// In the streaming case, the process stays running in the
	// foreground and prints out connections to the HostPort.
	//
	// The local backend handles updating the ServeConfig as
	// necessary, then restores it to its original state once
	// the process's context is closed or the client turns off
	// Tailscale.
	return e.streamServe(ctx, ipn.ServeStreamRequest{
		HostPort:   hp,
		Source:     source,
		MountPoint: "/", // TODO(marwan-at-work): support multiple mount points
	})
}

func (e *serveEnv) streamServe(ctx context.Context, req ipn.ServeStreamRequest) error {
	stream, err := e.lc.StreamServe(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()

	printHeader(req.HostPort)

	var logs []ipn.FunnelRequestLog
	maxLogs := 25
	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		var entry ipn.FunnelRequestLog
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return err
		}

		logs = append([]ipn.FunnelRequestLog{entry}, logs...)
		if len(logs) > maxLogs {
			logs = logs[:maxLogs]
		}

		printHeader(req.HostPort)

		for _, log := range logs {
			printLog(log)
		}
	}

	return nil
}

func printLog(log ipn.FunnelRequestLog) {
	host, _, err := net.SplitHostPort(log.SrcAddr.String())
	if err != nil {
		host = log.SrcAddr.String()
	}

	firstColumn := fmt.Sprintf("%s %s", log.Method, log.Path)
	secondColumn := host
	if log.NodeName != "" {
		secondColumn += " " + log.NodeName
	}
	if log.UserLoginName != "" {
		secondColumn += " " + color.GreenString(log.UserLoginName)
	}

	fmt.Printf("%-30s\t%s\n", firstColumn, secondColumn)
}

func printHeader(hostPort ipn.HostPort) {
	// Clear the screen
	fmt.Print("\033[H\033[2J")

	fmt.Printf(`
Available at "https://%s"
Press Ctrl-C to stop
`, strings.TrimSuffix(string(hostPort), ":443"))
}
