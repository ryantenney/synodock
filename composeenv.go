package main

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// composeService is what a single-service deploy reads of a compose service: enough to
// tell whether building the project would touch anything but the one service.
type composeService struct {
	Image         string   `yaml:"image"`
	ContainerName string   `yaml:"container_name"`
	Build         any      `yaml:"build"`
	DependsOn     any      `yaml:"depends_on"` // a list, or a map of service -> condition
	NetworkMode   string   `yaml:"network_mode"`
	VolumesFrom   []string `yaml:"volumes_from"`
	EnvFile       any      `yaml:"env_file"`    // a path, a list of paths, or of {path: ...}
	Environment   any      `yaml:"environment"` // a list of K=V, or a map
	Profiles      []string `yaml:"profiles"`
}

func parseServices(text string) (map[string]composeService, error) {
	var file struct {
		Services map[string]composeService `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(text), &file); err != nil {
		return nil, fmt.Errorf("compose file: %w", err)
	}
	return file.Services, nil
}

// imageRef is an image reference with Docker's implicit :latest.
func imageRef(ref string) string {
	if strings.Contains(ref, "@") || strings.Contains(ref[strings.LastIndex(ref, "/")+1:], ":") {
		return ref
	}
	return ref + ":latest"
}

// dependsOn is the services s waits on, from either form of depends_on.
func (s composeService) dependsOn() []string {
	var out []string
	switch d := s.DependsOn.(type) {
	case []any:
		for _, v := range d {
			out = append(out, fmt.Sprint(v))
		}
	case map[string]any:
		for k := range d {
			out = append(out, k)
		}
	}
	return out
}

// envFiles is the service's env_file paths, relative to the project folder.
func (s composeService) envFiles() []string {
	var raw []any
	switch e := s.EnvFile.(type) {
	case string, map[string]any:
		raw = []any{e}
	case []any:
		raw = e
	}
	var out []string
	for _, f := range raw {
		p := ""
		switch v := f.(type) {
		case string:
			p = v
		case map[string]any:
			p = fmt.Sprint(v["path"])
		}
		out = append(out, strings.TrimPrefix(p, "./"))
	}
	return out
}

var envLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)
var envComment = regexp.MustCompile(`\s+#.*$`)

// dotenv reads an env file as compose does: KEY -> value. A nil value is one compose would
// interpolate ($ outside single quotes), which isn't compared.
func dotenv(text string) map[string]*string {
	out := map[string]*string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		m := envLine.FindStringSubmatchIndex(line)
		if m == nil {
			continue
		}
		key := line[m[2]:m[3]]
		raw := strings.TrimSpace(line[m[1]:])
		if len(raw) > 1 && raw[0] == '\'' && raw[len(raw)-1] == '\'' {
			v := raw[1 : len(raw)-1]
			out[key] = &v
			continue
		}
		if len(raw) > 1 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			raw = raw[1 : len(raw)-1]
		} else {
			raw = envComment.ReplaceAllString(raw, "")
		}
		if strings.Contains(raw, "$") {
			out[key] = nil
		} else {
			v := raw
			out[key] = &v
		}
	}
	return out
}

var interpolation = regexp.MustCompile(`\$(?:\$|\{([A-Za-z_][A-Za-z0-9_]*)(?:(:?[-?+])([^}]*))?\}|([A-Za-z_][A-Za-z0-9_]*))`)

// interpolate applies compose's $VAR, ${VAR}, ${VAR:-default} (and -, :+, +, :?, ?) from
// the project's .env. ok is false when the result depends on a value that isn't known.
func interpolate(value string, env map[string]*string) (string, bool) {
	known := true
	var b strings.Builder
	last := 0
	for _, m := range interpolation.FindAllStringSubmatchIndex(value, -1) {
		b.WriteString(value[last:m[0]])
		last = m[1]
		if value[m[0]:m[1]] == "$$" {
			b.WriteString("$")
			continue
		}
		group := func(i int) string {
			if m[2*i] < 0 {
				return ""
			}
			return value[m[2*i]:m[2*i+1]]
		}
		name, op, arg := group(1), group(2), group(3)
		if name == "" {
			name = group(4)
		}
		v, isSet := env[name]
		if strings.Contains(arg, "$") || (isSet && v == nil) {
			known = false
			continue
		}
		val := ""
		if v != nil {
			val = *v
		}
		switch op {
		case ":-":
			if val == "" {
				val = arg
			}
		case "-":
			if !isSet {
				val = arg
			}
		case ":+":
			if val != "" {
				val = arg
			}
		case "+":
			val = ""
			if isSet {
				val = arg
			}
		}
		b.WriteString(val) // plain, or ?/:? (compose refuses to run when unset)
	}
	b.WriteString(value[last:])
	return b.String(), known
}

// expectedEnv is the environment compose gives the service's container: its env files,
// then environment:, interpolated from the project's .env. A nil value isn't compared.
func (s composeService) expectedEnv(projectEnv map[string]*string, envFiles map[string]map[string]*string) map[string]*string {
	out := map[string]*string{}
	for _, path := range s.envFiles() {
		for k, v := range envFiles[path] {
			out[k] = v
		}
	}
	set := func(key string, value any, fromShell bool) {
		if fromShell { // taken from the shell compose runs in, which isn't known here
			delete(out, key)
			return
		}
		str := fmt.Sprint(value)
		if b, ok := value.(bool); ok {
			str = fmt.Sprint(b) // true/false, as compose writes them
		}
		if v, ok := interpolate(str, projectEnv); ok {
			out[key] = &v
		} else {
			out[key] = nil
		}
	}
	switch e := s.Environment.(type) {
	case []any:
		for _, item := range e {
			k, v, found := strings.Cut(fmt.Sprint(item), "=")
			set(k, v, !found)
		}
	case map[string]any:
		for k, v := range e {
			set(k, v, v == nil)
		}
	}
	return out
}
