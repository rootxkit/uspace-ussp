package config

import (
	"errors"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/rootxkit/uspace-core/core"
)

// LookupFunc reads one environment variable: os.LookupEnv in
// production, a map in tests.
type LookupFunc func(name string) (string, bool)

const redactedValue = "<redacted>"

// field is one tagged struct field.
type field struct {
	v    reflect.Value
	sf   reflect.StructField
	name string
}

func (f field) tag(k string) string { return f.sf.Tag.Get(k) }

func (f field) empty() bool { return f.v.IsZero() }

func listed(tag, process string) bool {
	if tag == "" {
		return false
	}
	return tag == "all" || slices.Contains(strings.Split(tag, ","), process)
}

func (f field) neededBy(process string) bool { return listed(f.tag("need"), process) }

func (f field) neededFor(processes []string) []string {
	var out []string
	for _, p := range processes {
		if f.neededBy(p) {
			out = append(out, p)
		}
	}
	return out
}

// each calls fn for every tagged field of *Config in declaration order.
func each(c *Config, fn func(field)) {
	v := reflect.ValueOf(c).Elem()
	t := v.Type()
	for i := range t.NumField() {
		sf := t.Field(i)
		name, ok := sf.Tag.Lookup("env")
		if !ok {
			continue
		}
		fn(field{v: v.Field(i), sf: sf, name: name})
	}
}

func load(c *Config, lookup LookupFunc) error {
	var errs []error
	each(c, func(f field) {
		if err := loadField(f, lookup); err != nil {
			errs = append(errs, err)
		}
	})
	return errors.Join(errs...)
}

func loadField(f field, lookup LookupFunc) error {
	raw, _ := lookup(f.name)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = f.tag("default")
		if raw == "" {
			return nil
		}
	}
	if enum := f.tag("enum"); enum != "" && !slices.Contains(strings.Split(enum, "|"), raw) {
		return core.Fieldf(f.name, "must be one of %s", strings.ReplaceAll(enum, "|", ", "))
	}
	if err := set(f, raw); err != nil {
		return err
	}
	switch f.tag("kind") {
	case "url":
		if err := checkURL(raw); err != nil {
			return &core.FieldError{Field: f.name, Reason: err.Error()}
		}
	case "issuers":
		if _, err := ParseIssuers(f.v.Interface().([]string)); err != nil {
			return &core.FieldError{Field: f.name, Reason: err.Error()}
		}
	case "cidrs":
		for _, s := range f.v.Interface().([]string) {
			if _, err := netip.ParsePrefix(s); err == nil {
				continue
			}
			if _, err := netip.ParseAddr(s); err != nil {
				return core.Fieldf(f.name, "%q is neither a CIDR nor an address", s)
			}
		}
	}
	return nil
}

func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("is not a URL")
	}
	if u.Scheme == "" || u.Host == "" {
		return errors.New("must be an absolute URL with a scheme and a host")
	}
	return nil
}

func set(f field, raw string) error {
	switch p := f.v.Addr().Interface().(type) {
	case *string:
		*p = raw
	case *int:
		n, err := strconv.Atoi(raw)
		if err != nil {
			return core.Fieldf(f.name, "must be an integer")
		}
		if s := f.tag("min"); s != "" {
			if lo, _ := strconv.Atoi(s); n < lo {
				return core.Fieldf(f.name, "must be at least %s", s)
			}
		}
		if s := f.tag("max"); s != "" {
			if hi, _ := strconv.Atoi(s); n > hi {
				return core.Fieldf(f.name, "must be at most %s", s)
			}
		}
		*p = n
	case *[]string:
		var out []string
		for part := range strings.SplitSeq(raw, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
		*p = out
	default:
		return core.Fieldf(f.name, "unsupported field type %s", f.v.Type())
	}
	return nil
}

// render is the logged form of one value.
func render(f field) string {
	var val string
	if f.v.Kind() == reflect.Slice {
		val = strings.Join(f.v.Interface().([]string), ",")
	} else {
		val = strconv.Quote(f.v.String())
		if f.v.Kind() == reflect.Int {
			val = strconv.FormatInt(f.v.Int(), 10)
		}
	}
	switch f.tag("secret") {
	case "true":
		if !f.empty() {
			val = redactedValue
		}
	case "url":
		if u, err := url.Parse(f.v.String()); err == nil && u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				u.User = url.UserPassword(u.User.Username(), "xxxxx")
			} else {
				u.User = url.User("xxxxx")
			}
			val = strconv.Quote(u.String())
		} else if err != nil {
			val = redactedValue
		}
	}
	return val
}

func describe(c *Config) string {
	var parts []string
	each(c, func(f field) { parts = append(parts, f.name+"="+render(f)) })
	return strings.Join(parts, " ")
}

// Variable is one documented variable, for --help and deploy/ENV.md.
type Variable struct {
	Name     string
	ReadBy   string // "all" or a comma list of processes
	Required string // processes that need it; empty when optional
	Default  string
	Unit     string
	Help     string
}

// Variables lists every variable Config reads, in declaration order.
func Variables() []Variable {
	var out []Variable
	each(&Config{}, func(f field) {
		out = append(out, Variable{
			Name: f.name, ReadBy: f.tag("by"), Required: f.tag("need"),
			Default: f.tag("default"), Unit: f.tag("unit"), Help: f.tag("help"),
		})
	})
	return out
}

// Help lists the variables process reads, one per line, for --help.
func Help(process string) string {
	var b strings.Builder
	for _, v := range Variables() {
		if !listed(v.ReadBy, process) {
			continue
		}
		b.WriteString(v.Name)
		var notes []string
		if listed(v.Required, process) {
			notes = append(notes, "required")
		}
		if v.Default != "" {
			notes = append(notes, "default "+v.Default)
		}
		if v.Unit != "" {
			notes = append(notes, "unit "+v.Unit)
		}
		if len(notes) > 0 {
			b.WriteString(" (" + strings.Join(notes, "; ") + ")")
		}
		b.WriteString("\n    " + v.Help + "\n")
	}
	return b.String()
}

// FieldErrors flattens an error of LoadFrom or Require into its
// *core.FieldError parts.
func FieldErrors(err error) []*core.FieldError {
	if err == nil {
		return nil
	}
	var j interface{ Unwrap() []error }
	if errors.As(err, &j) {
		var out []*core.FieldError
		for _, e := range j.Unwrap() {
			out = append(out, FieldErrors(e)...)
		}
		return out
	}
	var fe *core.FieldError
	if errors.As(err, &fe) {
		return []*core.FieldError{fe}
	}
	return nil
}
