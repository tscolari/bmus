package tracker

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Opener builds an adapter from its section of tracker.yaml, nil when the
// file has none. It should decode the section strictly (KnownFields).
type Opener func(section *yaml.Node) (Tracker, error)

var (
	registryMu sync.Mutex
	registry   = map[string]Opener{}
)

// Register makes adapter name available to tracker.yaml. Adapters call it
// from init; a second registration of a name panics.
func Register(name string, open Opener) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[name]; dup {
		panic("tracker: adapter " + name + " registered twice")
	}
	registry[name] = open
}

// Adapters lists the registered adapters' names, sorted.
func Adapters() []string {
	registryMu.Lock()
	defer registryMu.Unlock()
	var names []string
	for n := range registry {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// Open builds the adapter tracker.yaml names from data:
//
//	adapter: github
//	github:
//	  repo: owner/name
//
// The file holds one YAML document with an adapter key and at most one
// other key, the adapter's own section, named after it.
func Open(data []byte) (Tracker, error) {
	var doc map[string]yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("empty tracker file: it needs an adapter")
		}
		return nil, err
	}
	// Only empty documents may follow, e.g. after a trailing "---".
	for {
		var extra any
		err := dec.Decode(&extra)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || extra != nil {
			return nil, errors.New("a tracker file holds one YAML document")
		}
	}
	an, ok := doc["adapter"]
	if !ok {
		return nil, fmt.Errorf("adapter is required (one of %s)", strings.Join(Adapters(), ", "))
	}
	var name string
	if err := an.Decode(&name); err != nil || name == "" {
		return nil, errors.New("adapter must be a non-empty string")
	}
	registryMu.Lock()
	open, ok := registry[name]
	registryMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown adapter %q (registered: %s)", name, strings.Join(Adapters(), ", "))
	}
	var section *yaml.Node
	for k, v := range doc {
		switch k {
		case "adapter":
		case name:
			section = &v
		default:
			return nil, fmt.Errorf("unknown key %q: a tracker file has adapter and a %s section", k, name)
		}
	}
	t, err := open(section)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return t, nil
}

// Load reads tracker.yaml at path and opens its adapter. A missing file is
// no tracker: found false, no error.
func Load(path string) (t Tracker, found bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	t, err = Open(data)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return t, true, nil
}

// DecodeSection decodes an adapter's tracker.yaml section into v,
// refusing unknown fields. A nil section leaves v as it is.
func DecodeSection(section *yaml.Node, v any) error {
	if section == nil {
		return nil
	}
	data, err := yaml.Marshal(section)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
