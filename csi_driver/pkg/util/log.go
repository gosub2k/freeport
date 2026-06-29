package util

import "log/slog"

var Log = slog.Default()

func UpdateLog(l* slog.Logger) {
	slog.SetDefault(l)
}