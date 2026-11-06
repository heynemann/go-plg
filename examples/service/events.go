package main

import (
	"context"
	"log/slog"

	"github.com/heynemann/go-plg"
)

// logEvent returns the service's plg.WithOnEvent sink: every plugin lifecycle event becomes a log record naming
// the plugin and the team to tell, at Warn for a rejection or a panic. A real service would page or post to the
// owner's contact.oncall or contact.channel instead.
func logEvent(logger *slog.Logger) func(context.Context, plg.Event) {
	return func(ctx context.Context, event plg.Event) {
		level := slog.LevelInfo
		attrs := []slog.Attr{
			slog.String("kind", string(event.Kind)),
			slog.String("key", event.Key),
			slog.String("version", event.Version),
			slog.String("team", event.Owner.Team.Name),
			slog.String("oncall", event.Owner.Contact.Oncall),
			slog.String("channel", event.Owner.Contact.Channel),
		}

		var failure error

		switch {
		case event.Kind == plg.EventPanicked:
			failure = event.Panic
		case event.Report != nil:
			failure = event.Report.Err
		default:
		}

		if failure != nil {
			level = slog.LevelWarn

			attrs = append(attrs, slog.Any("error", failure))
		}

		logger.LogAttrs(ctx, level, "plugin event", attrs...)
	}
}
