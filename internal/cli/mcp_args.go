package cli

import "time"

// The arguments every tool takes. They are types rather than a loose map because
// the type IS the input schema: the protocol derives the schema from these
// fields, so a field's doc comment is the description an agent reads for it and
// there is no second copy of the argument list to fall out of date.

// projectArgs is a tool that needs a project directory and nothing else.
type projectArgs struct {
	// Project is a directory holding a cafaye.yml. It defaults to the server's
	// working directory, which is the project an agent host usually spawns caf
	// in.
	Project string `json:"project,omitempty" jsonschema:"directory holding the cafaye.yml to read; the server's working directory when omitted"`
}

// planArgs is a tool that plans or starts a stack, which is `caf dev`'s own
// flag set minus the two that only make sense on a terminal.
type planArgs struct {
	// Project is a directory holding a cafaye.yml.
	Project string `json:"project,omitempty" jsonschema:"project directory to plan; the server's working directory when omitted"`
	// Out is where the rendered compose document is written, relative to the
	// project unless absolute.
	Out string `json:"out,omitempty" jsonschema:"file to write the compose document to, inside the project; caf.dev.compose.yaml by default"`
	// Port publishes the project's own service on this host port instead of the
	// one its image listens on.
	Port int `json:"port,omitempty" jsonschema:"host port to publish the project service on; 0 uses the service's own port"`
	// NoInfra leaves out the local postgres and redis.
	NoInfra bool `json:"no_infra,omitempty" jsonschema:"leave out the local postgres and redis this platform adds by default"`
	// Wait bounds how long caf_dev_up waits for the stack to settle. It is an
	// argument rather than a constant because a first run builds images and
	// pulls databases, and an agent driving a cold machine needs to be able to
	// wait longer than a person would.
	Wait time.Duration `json:"wait,omitempty" jsonschema:"how long to wait for the stack to come up, as a Go duration such as 3m"`
}

// downArgs is a tool that only needs to know which project.
type downArgs struct {
	// Project is a directory holding a cafaye.yml.
	Project string `json:"project,omitempty" jsonschema:"project whose stack to stop; the server's working directory when omitted"`
}

// doctorArgs is the one tool that reports about a machine rather than a project.
type doctorArgs struct {
	// Project is the directory whose requirements are checked.
	Project string `json:"project,omitempty" jsonschema:"project directory to check; the server's working directory when omitted"`
	// ToolsOnly skips the project half and reports the toolchain table alone.
	ToolsOnly bool `json:"tools_only,omitempty" jsonschema:"report only the toolchain table, skipping the project checks"`
}
