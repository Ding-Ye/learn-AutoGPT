// component_filemgr.go — FileManagerComponent.
//
// FileManagerComponent is the canonical example of a component that
// implements TWO protocols: CommandProvider (emits read_file +
// write_file tools) AND DirectiveProvider (emits two policy lines
// reminding the model how to use them safely).
//
// AutoGPT upstream's `forge/components/file_manager/__init__.py` is
// roughly the same shape: it wraps a FileStorage, registers a handful
// of file-related commands (`open_file`, `write_file`, `list_folder`,
// etc.), and contributes a few directive lines via
// `get_constraints`. We ship two tools (read + write — that's all the
// agent needs to do useful work) and two directives.
//
// Why "wrap a Workspace" rather than re-derive paths? Because s06's
// Workspace interface is the seam: any file-touching tool takes a
// Workspace at construction. This means a future component
// (S3Workspace, EncryptedWorkspace) would slot in without changing
// the Component shape — the Component just gets a different Workspace.
package main

// FileManagerComponent bundles file-related capabilities into one
// Component. The Workspace is captured at construction; the same
// instance is wired into both the Read and Write tool, so the two
// tools share sandbox state.
type FileManagerComponent struct {
	ws Workspace
}

// NewFileManagerComponent builds a component over the given Workspace.
// Returns *FileManagerComponent (not a Component) so callers can
// inspect or extend before wrapping into a slice.
func NewFileManagerComponent(ws Workspace) *FileManagerComponent {
	return &FileManagerComponent{ws: ws}
}

// Commands implements CommandProvider. The order is deliberate:
// read_file before write_file, because the directives below remind the
// model to "read before edit" and the tool listing should match that
// expectation. ComponentBus preserves this order when building the
// Registry, so the system prompt's "## Commands" section also has
// read_file first.
func (f *FileManagerComponent) Commands() []Tool {
	return []Tool{
		NewReadFileTool(f.ws),
		NewWriteFileTool(f.ws),
	}
}

// Directives implements DirectiveProvider. Two lines:
//
//  1. "Always read a file before editing it." — protects against the
//     "model writes a brand-new file with content that overwrites all
//     of the existing version" failure mode.
//  2. "Use list_files to discover before reading." — list_files isn't
//     yet a tool (we ship only Read+Write); the line is forward-compat
//     so upgrading to a list-capable workspace later doesn't require
//     touching this directive set.
//
// AutoGPT upstream's analogous directives are longer but the same
// shape. We keep ours short because the OneShotStrategy renders them
// into a numbered list and short bullets read better.
func (f *FileManagerComponent) Directives() []string {
	return []string{
		"Always read a file before editing it.",
		"Use list_files to discover before reading.",
	}
}

// Compile-time assertions — FileManagerComponent satisfies both
// CommandProvider and DirectiveProvider, but NOT MessageProvider.
// (The bus's type-assertion will skip it for Messages, which is
// what makes "implement only what you need" work.)
var (
	_ Component         = (*FileManagerComponent)(nil)
	_ CommandProvider   = (*FileManagerComponent)(nil)
	_ DirectiveProvider = (*FileManagerComponent)(nil)
)
