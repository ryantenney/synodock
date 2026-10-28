package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// imageLine finds each `image:` in a compose file, as dsm.py does: a plain text match,
// since a compose file's variables aren't interpolated for this.
var imageLine = regexp.MustCompile(`(?m)^\s*image:\s*['"]?([^'"\s#]+)`)

// composeImages are the image references a compose file runs, with Docker's implicit
// :latest added.
func composeImages(text string) []string {
	var out []string
	for _, m := range imageLine.FindAllStringSubmatch(text, -1) {
		ref := m[1]
		if last := ref[strings.LastIndex(ref, "/")+1:]; !strings.Contains(last, ":") {
			ref += ":latest"
		}
		out = append(out, ref)
	}
	return out
}

// composeContainerNames are the containers a compose file runs: container_name, else
// compose's <project>-<service>-1. Services that start only under a profile are left out,
// since a build doesn't start them.
func composeContainerNames(text, projectName string) ([]string, error) {
	var file struct {
		Name     string `yaml:"name"`
		Services map[string]struct {
			ContainerName string   `yaml:"container_name"`
			Profiles      []string `yaml:"profiles"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(text), &file); err != nil {
		return nil, fmt.Errorf("compose file: %w", err)
	}
	var names []string
	for service, svc := range file.Services {
		if len(svc.Profiles) > 0 {
			continue
		}
		name := svc.ContainerName
		if name == "" {
			name = composeProjectName(firstNonEmpty(file.Name, projectName)) + "-" + service + "-1"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// composeProjectName is a project name as compose normalises it: lowercase, keeping only
// letters, digits, - and _ (so "My Stack" is "mystack").
func composeProjectName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
