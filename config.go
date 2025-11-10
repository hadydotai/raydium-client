package main

/*

NOTE(@hadydotai): A copy from my validation code which I should probably release at some point as a library but alas, I only need some bits
of it here. I should probably also include the env handling but this will do for now.

*/

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
)

// FlagRule represents a validation rule that runs against a flag entry.
type FlagRule func(spec *FlagSpec, ctx *validationContext) error

// FlagSpec bundles a flag name, its backing pointer, and the rules to enforce on it.
type FlagSpec struct {
	Name  string
	Value any
	Rules []FlagRule
}

// ValidateConfigOrExit validates the provided specs and prints the help output on failure.
func ValidateConfigOrExit(fs *flag.FlagSet, specs []FlagSpec) {
	if err := runFlagValidations(specs); err != nil {
		if fs == nil {
			fs = flag.CommandLine
		}
		fmt.Fprintf(os.Stderr, "configuration error: %v\n\n", err)
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fs.PrintDefaults()
		os.Exit(2)
	}
}

// NotEmpty asserts that the underlying string flag is not blank.
func NotEmpty() FlagRule {
	return func(spec *FlagSpec, ctx *validationContext) error {
		value, ok := stringValue(spec.Value)
		if !ok {
			return fmt.Errorf("flag -%s must be a string", spec.Name)
		}
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("flag -%s must not be empty", spec.Name)
		}
		return nil
	}
}

// OneOf asserts that a string flag is one of the provided options (case-insensitive).
func OneOf(options ...string) FlagRule {
	allowed := make(map[string]struct{}, len(options))
	for _, opt := range options {
		allowed[strings.ToLower(strings.TrimSpace(opt))] = struct{}{}
	}
	return func(spec *FlagSpec, ctx *validationContext) error {
		value, ok := stringValue(spec.Value)
		if !ok {
			return fmt.Errorf("flag -%s must be a string", spec.Name)
		}
		normalized := strings.ToLower(strings.TrimSpace(value))
		if _, exists := allowed[normalized]; !exists {
			choices := make([]string, 0, len(allowed))
			for opt := range allowed {
				choices = append(choices, opt)
			}
			sort.Strings(choices)
			return fmt.Errorf("flag -%s must be one of [%s]", spec.Name, strings.Join(choices, ", "))
		}
		return nil
	}
}

// Requires ensures that when the current flag is set, the dependent flag passes validation.
func Requires(dep string) FlagRule {
	return func(spec *FlagSpec, ctx *validationContext) error {
		if !valueProvided(spec.Value) {
			return nil
		}
		target, ok := ctx.registry[dep]
		if !ok {
			return fmt.Errorf("flag -%s requires -%s, but the dependency is not registered", spec.Name, dep)
		}
		if err := ctx.validate(target); err != nil {
			return fmt.Errorf("flag -%s requires -%s: %w", spec.Name, dep, err)
		}
		return nil
	}
}

type validationContext struct {
	registry   map[string]*FlagSpec
	validating map[string]bool
	validated  map[string]bool
}

func runFlagValidations(specs []FlagSpec) error {
	if len(specs) == 0 {
		return nil
	}
	ctx := &validationContext{
		registry:   make(map[string]*FlagSpec, len(specs)),
		validating: make(map[string]bool, len(specs)),
		validated:  make(map[string]bool, len(specs)),
	}
	for i := range specs {
		spec := &specs[i]
		if spec.Name == "" {
			return errors.New("flag spec missing name")
		}
		if spec.Value == nil {
			return fmt.Errorf("flag -%s is missing its backing pointer", spec.Name)
		}
		if _, exists := ctx.registry[spec.Name]; exists {
			return fmt.Errorf("flag -%s defined more than once", spec.Name)
		}
		ctx.registry[spec.Name] = spec
	}
	for _, spec := range ctx.registry {
		if err := ctx.validate(spec); err != nil {
			return err
		}
	}
	return nil
}

func (ctx *validationContext) validate(spec *FlagSpec) error {
	if spec == nil {
		return nil
	}
	if ctx.validated[spec.Name] {
		return nil
	}
	if ctx.validating[spec.Name] {
		return nil
	}
	ctx.validating[spec.Name] = true
	defer delete(ctx.validating, spec.Name)
	for _, rule := range spec.Rules {
		if rule == nil {
			continue
		}
		if err := rule(spec, ctx); err != nil {
			return err
		}
	}
	ctx.validated[spec.Name] = true
	return nil
}

func stringValue(value any) (string, bool) {
	rv, ok := derefValue(value)
	if !ok || rv.Kind() != reflect.String {
		return "", false
	}
	return rv.String(), true
}

func valueProvided(value any) bool {
	rv, ok := derefValue(value)
	if !ok {
		return false
	}
	switch rv.Kind() {
	case reflect.String:
		return strings.TrimSpace(rv.String()) != ""
	case reflect.Bool:
		return rv.Bool()
	default:
		return !rv.IsZero()
	}
}

func derefValue(value any) (reflect.Value, bool) {
	if value == nil {
		return reflect.Value{}, false
	}
	rv := reflect.ValueOf(value)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return reflect.Value{}, false
		}
		rv = rv.Elem()
	}
	if !rv.IsValid() {
		return reflect.Value{}, false
	}
	return rv, true
}
