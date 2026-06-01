package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"strings"

	"github.com/opentalon/opentalon/pkg/channel"
	wschannel "github.com/opentalon/websocket-channel/channel"
)

func main() {
	os.Exit(run())
}

func run() int {
	addr := flag.String("addr", "0.0.0.0:9000", "WebSocket server address (host:port)")
	path := flag.String("path", "/ws", "WebSocket endpoint path")
	origins := flag.String("origins", "", "Comma-separated allowed CORS origins (empty = allow all)")
	whoamiURL := flag.String("whoami-url", "", "Timly /whoami endpoint for identity resolution (required)")
	whoamiSecret := flag.String("whoami-secret", "", "Shared secret for /whoami (X-Security-Token); falls back to WHOAMI_SECRET env")
	flag.Parse()

	cfg := wschannel.Config{
		Addr:         *addr,
		Path:         *path,
		WhoamiURL:    *whoamiURL,
		WhoamiSecret: *whoamiSecret,
	}
	if *origins != "" {
		for _, o := range strings.Split(*origins, ",") {
			o = strings.TrimSpace(o)
			if o != "" {
				cfg.CORSOrigins = append(cfg.CORSOrigins, o)
			}
		}
	}

	ch := wschannel.New(cfg)
	defer func() { _ = ch.Stop() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := channel.Serve(ctx, ch); err != nil && ctx.Err() == nil {
		_, _ = os.Stderr.WriteString("channel serve: " + err.Error() + "\n")
		return 1
	}
	return 0
}
