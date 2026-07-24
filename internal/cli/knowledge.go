package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/ryanthedev/engram/internal/mcp"
)

// --- knowledge collection admin (gRPC-backed) ---
//
// Collections must exist before a harvest run: the knowledge index is
// dynamic:strict, so a source emitting a field the collection never declared
// has that document rejected at ingest. Registration was previously reachable
// only through the knowledge_create_collection MCP tool, which leaves an
// operator holding an admin token but no way to spend it — every other admin
// operation (token, acl, quarantine) has a CLI verb. These subcommands close
// that gap so a collection can be provisioned from the same shell that mints
// the token and runs the harvester.

func runKnowledge(ctx context.Context, args []string, env Env, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("knowledge: expected collections|create-collection")
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "collections":
		return runKnowledgeCollections(ctx, rest, env, out)
	case "create-collection":
		return runKnowledgeCreateCollection(ctx, rest, env, out)
	default:
		return fmt.Errorf("knowledge: unknown subcommand %q", sub)
	}
}

func runKnowledgeCollections(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("knowledge collections", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "", "engramd address")
	token := fs.String("token", "", "bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	client, err := dialClient(env, *addr, *token)
	if err != nil {
		return err
	}
	defer client.Close()
	infos, err := client.KnowledgeCollections(ctx)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(infos, "", "  ")
	fmt.Fprintln(out, string(b))
	return nil
}

func runKnowledgeCreateCollection(ctx context.Context, args []string, env Env, out io.Writer) error {
	fs := flag.NewFlagSet("knowledge create-collection", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addr := fs.String("addr", "", "engramd address")
	token := fs.String("token", "", "bearer token")
	name := fs.String("name", "", "collection name (required)")
	textField := fs.String("text-field", "", "document field BM25 indexes as the body (server default: text)")
	public := fs.Bool("public", false, "readable by any authenticated caller")
	roles := fs.String("roles", "", "comma-separated read roles when not public")
	var fields fieldSpecFlag
	fs.Var(&fields, "field", "field mapping NAME:TYPE[:filterable][:sortable] (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("create-collection: --name is required")
	}
	spec := mcp.CollectionSpec{
		Name:      strings.TrimSpace(*name),
		TextField: strings.TrimSpace(*textField),
		Mappings:  fields.mappings,
		Public:    *public,
		Roles:     parseRoles(*roles),
	}
	client, err := dialClient(env, *addr, *token)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.CreateCollection(ctx, spec); err != nil {
		return err
	}
	fmt.Fprintf(out, "collection %q created (%d declared field(s))\n", spec.Name, len(spec.Mappings))
	return nil
}

// fieldSpecFlag accumulates repeated --field values into a mappings map. The
// value grammar is NAME:TYPE[:filterable][:sortable] — colon-separated rather
// than JSON so a collection can be declared inline in a shell command without
// quoting a nested object, which is the whole point of having the verb.
type fieldSpecFlag struct {
	mappings map[string]mcp.FieldSpec
}

func (f *fieldSpecFlag) String() string {
	if len(f.mappings) == 0 {
		return ""
	}
	names := make([]string, 0, len(f.mappings))
	for n := range f.mappings {
		names = append(names, n)
	}
	return strings.Join(names, ",")
}

// Set parses one --field occurrence (NAME:TYPE[:filterable][:sortable]) and
// adds it to the mappings. A duplicate name is an error rather than a silent
// last-wins overwrite: two --field flags naming the same field mean the
// operator's intent is ambiguous, and the collection's mapping is not the
// place to guess.
func (f *fieldSpecFlag) Set(v string) error {
	parts := strings.Split(v, ":")
	if len(parts) < 2 {
		return fmt.Errorf("field %q: want NAME:TYPE[:filterable][:sortable]", v)
	}
	name := strings.TrimSpace(parts[0])
	typ := strings.TrimSpace(parts[1])
	if name == "" || typ == "" {
		return fmt.Errorf("field %q: name and type must both be non-empty", v)
	}
	spec := mcp.FieldSpec{Type: typ}
	for _, opt := range parts[2:] {
		switch strings.TrimSpace(opt) {
		case "filterable":
			spec.Filterable = true
		case "sortable":
			spec.Sortable = true
		case "":
			// Tolerate a trailing colon rather than failing the whole run.
		default:
			return fmt.Errorf("field %q: unknown option %q (want filterable|sortable)", v, opt)
		}
	}
	if f.mappings == nil {
		f.mappings = make(map[string]mcp.FieldSpec)
	}
	if _, dup := f.mappings[name]; dup {
		return fmt.Errorf("field %q: declared twice", name)
	}
	f.mappings[name] = spec
	return nil
}
