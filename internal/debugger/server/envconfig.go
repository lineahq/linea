package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// The server config can be given as environment variables, with or without
// a file. One rule names them: LINEA_ plus the key's path in upper case, the
// segments joined by "_" (store.objects.bucket is LINEA_STORE_OBJECTS_BUCKET).
// The rule is applied to serverConfig's yaml tags, so a new key has its
// variable without a list to update. A variable overrides the file's value.
// Secrets have no variable of their own: their *_file keys are the variables
// that name the files.

const envPrefix = "LINEA_"

// envLookup reads one environment variable, as os.LookupEnv does.
type envLookup func(name string) (string, bool)

// environment is where the server config's overrides come from.
type environment struct {
	lookup envLookup
	// dir resolves the relative path a *_file variable holds; empty means the
	// working directory. A file's own relative paths resolve against the
	// file's directory.
	dir string
}

// processEnvironment is the environment of this process.
func processEnvironment() environment { return environment{lookup: os.LookupEnv} }

// override is one setting that came from an environment variable.
type override struct{ key, variable string }

// envName is the variable that sets a config key such as store.objects.bucket.
func envName(key string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// walkConfig visits every key a variable can set: the leaves of v, in field
// order, named by their yaml tags. Structs, and pointers to structs, are
// sections that hold their own keys; every other field is set whole, a list
// or a map as one YAML flow value. The top-level version is not a key: it
// belongs to the file format, and LINEA_VERSION names the release elsewhere.
// A section behind a nil pointer is allocated for the visit and kept only if
// leaf reports that it set something.
func walkConfig(v reflect.Value, path []string, leaf func(key string, field reflect.Value) (bool, error)) (bool, error) {
	set := false
	t := v.Type()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if name == "" || name == "-" || (len(path) == 0 && name == "version") {
			continue
		}
		key := append(slices.Clone(path), name)
		field := v.Field(i)
		var ok bool
		var err error
		switch {
		case field.Kind() == reflect.Struct:
			ok, err = walkConfig(field, key, leaf)
		case field.Kind() == reflect.Pointer && field.Type().Elem().Kind() == reflect.Struct:
			target := field
			if field.IsNil() {
				target = reflect.New(field.Type().Elem())
			}
			if ok, err = walkConfig(target.Elem(), key, leaf); ok && field.IsNil() {
				field.Set(target)
			}
		default:
			ok, err = leaf(strings.Join(key, "."), field)
		}
		if err != nil {
			return false, err
		}
		set = set || ok
	}
	return set, nil
}

// configKeys lists the key of every setting the environment can set.
func configKeys() []string {
	var keys []string
	_, _ = walkConfig(reflect.ValueOf(&serverConfig{}).Elem(), nil, func(key string, _ reflect.Value) (bool, error) {
		keys = append(keys, key)
		return false, nil
	})
	return keys
}

// apply sets the fields of cfg whose variable is set and not empty, and
// returns what it set. A variable that is set but empty counts as not set, so
// a Compose file may pass "${NAME:-}" through.
//
// A string is taken as it is. Any other scalar is read as YAML, as the file's
// value would be: true, 30, 12h inside a string key stays a string for the
// validation to parse. A list or a map is one YAML flow value:
// [{group: admins, role: admin, scope: "*"}]. A *_file value that is relative
// resolves against the working directory.
func (e environment) apply(cfg *serverConfig) ([]override, error) {
	var set []override
	_, err := walkConfig(reflect.ValueOf(cfg).Elem(), nil, func(key string, field reflect.Value) (bool, error) {
		name := envName(key)
		raw, ok := e.lookup(name)
		if raw = strings.TrimSpace(raw); !ok || raw == "" {
			return false, nil
		}
		if field.Kind() == reflect.String {
			if strings.HasSuffix(key, "_file") {
				var err error
				if raw, err = e.absolute(raw); err != nil {
					return false, fmt.Errorf("%s: %w", name, err)
				}
			}
			field.SetString(raw)
		} else {
			value := reflect.New(field.Type())
			dec := yaml.NewDecoder(strings.NewReader(raw))
			dec.KnownFields(true)
			if err := dec.Decode(value.Interface()); err != nil {
				return false, fmt.Errorf("%s: %s", name, envValueError(err, field.Type()))
			}
			field.Set(value.Elem())
		}
		set = append(set, override{key, name})
		return true, nil
	})
	return set, err
}

// absolute resolves a relative path of a *_file variable.
func (e environment) absolute(p string) (string, error) {
	if filepath.IsAbs(p) {
		return p, nil
	}
	dir := e.dir
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	return filepath.Join(dir, p), nil
}

// envValueError says what a variable of the given type wants, from a YAML
// decoding error.
func envValueError(err error, t reflect.Type) string {
	if errors.Is(err, io.EOF) {
		return "the value is empty"
	}
	detail := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(err.Error(), "yaml: "), "unmarshal errors:"))
	detail = strings.ReplaceAll(strings.ReplaceAll(detail, "\n  ", "; "), "line 1: ", "")
	if i := strings.Index(detail, " in type struct"); i >= 0 {
		detail = detail[:i]
	}
	switch t.Kind() {
	case reflect.Bool:
		return "give true or false"
	case reflect.Int, reflect.Pointer:
		return "give a whole number (" + detail + ")"
	case reflect.Slice, reflect.Map:
		return "give a YAML flow value such as [a, b] or [{key: value}] (" + detail + ")"
	}
	return detail
}

// nameVariables rewrites the keys an error message names to the variables
// that set them, for the keys that came from the environment: a value the
// operator did not write in a file is not found by its key.
func nameVariables(msg string, set []override) string {
	set = slices.Clone(set)
	slices.SortFunc(set, func(a, b override) int { return len(b.key) - len(a.key) })
	for _, o := range set {
		re := regexp.MustCompile(`(^|[^A-Za-z0-9_.])` + regexp.QuoteMeta(o.key) + `($|[^A-Za-z0-9_])`)
		msg = re.ReplaceAllString(msg, "${1}"+o.variable+"${2}")
	}
	return msg
}
