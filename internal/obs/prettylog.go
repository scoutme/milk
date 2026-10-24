package obs

import (
	"io"
	"log/slog"

	"charm.land/log/v2"
	"github.com/charmbracelet/colorprofile"
)

// LogFormatPretty is the milk.log format that writes colored, structured
// lines through Charm's log handler. Colors are forced on because the target
// is a file read with `tail -f` or `less -R`, never a TTY the handler could
// probe; use "text" or "json" for a plain file.
const LogFormatPretty = "pretty"

const prettyTimeFormat = "15:04:05.000"

// newPrettyHandler returns a slog handler that renders level, time, message
// and key/value attributes in color.
func newPrettyHandler(w io.Writer, level slog.Level) slog.Handler {
	logger := log.NewWithOptions(w, log.Options{
		Level:           log.Level(level),
		ReportTimestamp: true,
		TimeFormat:      prettyTimeFormat,
	})
	logger.SetColorProfile(colorprofile.TrueColor)
	return logger
}
