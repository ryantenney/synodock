//go:build !darwin && !linux

package service

import "errors"

const Manager = ""

var errUnsupported = errors.New("services are only supported on macOS (launchd) and Linux (systemd)")

func Label(name string) string                               { return "synodock" }
func LogPath(name string) string                             { return "" }
func Install(s Spec) ([]string, error)                       { return nil, errUnsupported }
func Uninstall(name string, socket string) ([]string, error) { return nil, errUnsupported }
func Status(name string) (string, error)                     { return "", errUnsupported }
