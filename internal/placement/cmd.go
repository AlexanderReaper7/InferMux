package placement

import (
	"slices"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// args is a command split as llama-swap splits it: the program and what
// precedes the first flag, then each flag with the values that follow it.
type args struct {
	head  []string
	flags []flagArg
}

type flagArg struct {
	name   string
	values []string
}

// The spellings llama.cpp accepts for the flags a device sets.
var (
	gpuLayerFlags = []string{"-ngl", "--n-gpu-layers", "--gpu-layers"}
	deviceFlags   = []string{"-dev", "--device"}
	modelFlags    = []string{"-m", "--model"}
	fileFlags     = []string{"-m", "--model", "-mm", "--mmproj", "-md", "--model-draft"}
)

// isFlag is a token that starts a flag: -x or --xyz, not a negative number.
func isFlag(token string) bool {
	if len(token) < 2 || token[0] != '-' {
		return false
	}
	c := token[1]
	return !(c >= '0' && c <= '9') && c != '.'
}

func splitCmd(cmd string) args {
	tokens, err := config.SanitizeCommand(cmd)
	if err != nil {
		return args{}
	}
	var a args
	for _, t := range tokens {
		switch {
		case isFlag(t):
			a.flags = append(a.flags, flagArg{name: t})
		case len(a.flags) == 0:
			a.head = append(a.head, t)
		default:
			last := &a.flags[len(a.flags)-1]
			last.values = append(last.values, t)
		}
	}
	return a
}

func (a args) has(name string) bool {
	for _, f := range a.flags {
		if f.name == name {
			return true
		}
	}
	return false
}

// value is the first value of the flag's last occurrence, the one llama.cpp
// keeps.
func (a args) value(name string) string {
	out := ""
	for _, f := range a.flags {
		if f.name == name {
			out = ""
			if len(f.values) > 0 {
				out = f.values[0]
			}
		}
	}
	return out
}

// without drops every occurrence of the names.
func (a args) without(names ...string) args {
	out := args{head: a.head}
	for _, f := range a.flags {
		if !slices.Contains(names, f.name) {
			out.flags = append(out.flags, f)
		}
	}
	return out
}

// set replaces the flag's first occurrence and drops the rest, or appends it.
func (a args) set(name string, values ...string) args {
	out := args{head: a.head}
	done := false
	for _, f := range a.flags {
		if f.name != name {
			out.flags = append(out.flags, f)
			continue
		}
		if !done {
			out.flags = append(out.flags, flagArg{name: name, values: values})
			done = true
		}
	}
	if !done {
		out.flags = append(out.flags, flagArg{name: name, values: values})
	}
	return out
}

func applyFlags(a args, overrides []Flag) args {
	for _, f := range overrides {
		if f.Present {
			a = a.set(f.Name, f.Values...)
		} else {
			a = a.without(f.Name)
		}
	}
	return a
}

// render writes one flag to a line, the way the model files are written.
func (a args) render() string {
	var b strings.Builder
	for i, t := range a.head {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(quote(t))
	}
	for _, f := range a.flags {
		b.WriteString("\n  ")
		b.WriteString(quote(f.name))
		for _, v := range f.values {
			b.WriteByte(' ')
			b.WriteString(quote(v))
		}
	}
	return b.String()
}

// quote leaves a token bare when shlex reads it back unchanged; ${...} stays
// bare so llama-swap's macros still see it.
func quote(t string) string {
	if t != "" && !strings.ContainsAny(t, " \t\n'\"\\#;&|<>()*?[]~`!") {
		return t
	}
	return "'" + strings.ReplaceAll(t, "'", `'\''`) + "'"
}

// ggufPaths is every GGUF the command names: the model, its projector and
// its draft.
func ggufPaths(cmd string) []string {
	a := splitCmd(cmd)
	var out []string
	for _, f := range a.flags {
		if slices.Contains(fileFlags, f.name) && len(f.values) > 0 && strings.HasSuffix(f.values[0], ".gguf") {
			out = append(out, f.values[0])
		}
	}
	return out
}

// isLlamaCpp is a command that loads a GGUF with --model: llama-server or a
// fork, which takes --device and -ngl. Others, such as the reranker and
// immich-ml, are kept off a card by the environment alone.
func isLlamaCpp(a args) bool {
	for _, name := range modelFlags {
		if strings.HasSuffix(a.value(name), ".gguf") {
			return true
		}
	}
	return false
}
