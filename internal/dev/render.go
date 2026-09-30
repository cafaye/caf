package dev

import (
	"fmt"
	"strconv"
	"strings"
)

// renderCompose writes the stack as a compose document.
//
// The document is the artifact a developer reads, diffs and pastes into an
// issue, so three things about it are deliberate rather than incidental. It is
// written in one fixed order — services by name, environment by name, volumes
// by name — so two runs of the same manifest produce the same bytes and a diff
// between them is a diff between two manifests. It carries no timestamp and
// nothing else that changes between runs, for the same reason. And it is
// written here, by hand, rather than marshalled: the standard library has no
// YAML encoder, and a general encoder would put this document's keys in
// whatever order its own map does, which is the one thing above that must not
// happen.
func renderCompose(project string, services []Service, volumes []string) string {
	var b strings.Builder

	// The header says what the file is and what not to do with it, because a
	// file called caf.dev.compose.yaml in a repository root is something a
	// developer will edit before reading it.
	fmt.Fprintf(&b, "# caf dev — the local stack for %q, generated from cafaye.yml.\n", project)
	b.WriteString("# Do not edit: every run rewrites this file. The manifest is the\n")
	b.WriteString("# source of truth; this is what it turned into.\n")
	fmt.Fprintf(&b, "name: %s\n\n", scalar(project))

	if len(services) > 0 {
		b.WriteString("services:\n")
		for _, svc := range services {
			writeService(&b, svc)
		}
	}
	if len(volumes) > 0 {
		// Every service already ended with the blank line that separates blocks,
		// so the top-level volumes heading follows one, not two.
		b.WriteString("volumes:\n")
		for _, name := range volumes {
			fmt.Fprintf(&b, "  %s: {}\n", scalar(name))
		}
	}
	return b.String()
}

// writeService writes one service. The key order is image, command, ports,
// environment, volumes, depends_on, healthcheck: what the container is, how it
// is started, what reaches it, what it is configured with, what it keeps, what
// it waits for, and how it says it is ready.
func writeService(b *strings.Builder, svc Service) {
	fmt.Fprintf(b, "  %s:\n", scalar(svc.Name))

	switch {
	case svc.Build != nil:
		b.WriteString("    build:\n")
		fmt.Fprintf(b, "      context: %s\n", scalar(svc.Build.Context))
		fmt.Fprintf(b, "      dockerfile: %s\n", scalar(svc.Build.Dockerfile))
		writeArgs(b, svc.Build.Args)
	case svc.Image != "":
		fmt.Fprintf(b, "    image: %s\n", scalar(svc.Image))
	}

	if len(svc.Command) > 0 {
		b.WriteString("    command:\n")
		for _, word := range svc.Command {
			fmt.Fprintf(b, "      - %s\n", scalar(word))
		}
	}
	if svc.Published > 0 {
		fmt.Fprintf(b, "    ports:\n      - %q\n", fmt.Sprintf("%d:%d", svc.Published, svc.Port))
	}
	if len(svc.Environment) > 0 {
		b.WriteString("    environment:\n")
		for _, env := range svc.Environment.Sorted() {
			fmt.Fprintf(b, "      %s: %s\n", scalar(env.Name), scalar(env.Value))
		}
	}
	if len(svc.Volumes) > 0 {
		b.WriteString("    volumes:\n")
		for _, volume := range svc.Volumes {
			fmt.Fprintf(b, "      - %s\n", scalar(volume))
		}
	}
	if len(svc.DependsOn) > 0 {
		b.WriteString("    depends_on:\n")
		for _, dep := range svc.DependsOn {
			fmt.Fprintf(b, "      %s:\n        condition: %s\n", scalar(dep.Name), dep.Condition)
		}
	}
	if svc.Healthcheck != nil {
		writeHealthcheck(b, svc.Healthcheck)
	}
	b.WriteString("\n")
}

func writeArgs(b *strings.Builder, args Environment) {
	if len(args) == 0 {
		return
	}
	b.WriteString("      args:\n")
	for _, arg := range args.Sorted() {
		fmt.Fprintf(b, "        %s: %s\n", scalar(arg.Name), scalar(arg.Value))
	}
}

func writeHealthcheck(b *strings.Builder, check *Healthcheck) {
	b.WriteString("    healthcheck:\n")
	if len(check.Test) > 0 {
		b.WriteString("      test:\n")
		for _, word := range check.Test {
			fmt.Fprintf(b, "        - %s\n", scalar(word))
		}
	}
	if check.Interval != "" {
		fmt.Fprintf(b, "      interval: %s\n", scalar(check.Interval))
	}
	if check.Timeout != "" {
		fmt.Fprintf(b, "      timeout: %s\n", scalar(check.Timeout))
	}
	if check.Retries > 0 {
		fmt.Fprintf(b, "      retries: %d\n", check.Retries)
	}
	if check.StartPeriod != "" {
		fmt.Fprintf(b, "      start_period: %s\n", scalar(check.StartPeriod))
	}
}

// scalar quotes a value only when it would otherwise be read as something other
// than the string it is. Two of those matter here and both have bitten a
// generated compose file: a connection string is fine plain, but a value that
// looks like a number would come back out of the parser as a number, and a
// value with a `: ` in it would end the scalar early and take the rest of the
// document with it. Everything else is written plain, because a document of
// quoted-everything is a document nobody reads.
func scalar(value string) string {
	if plainSafe(value) {
		return value
	}
	return strconv.Quote(value)
}

func plainSafe(value string) bool {
	if value == "" {
		return false
	}
	if strings.TrimSpace(value) != value {
		return false
	}
	if strings.ContainsAny(value, "\n\r\t") {
		return false
	}
	// A `: ` or ` #` ends a plain scalar mid-value, and a leading indicator
	// makes it something else entirely.
	if strings.Contains(value, ": ") || strings.Contains(value, " #") {
		return false
	}
	if strings.HasSuffix(value, ":") {
		return false
	}
	if strings.IndexByte("!&*[]{}|>%@`'\"?,#", value[0]) >= 0 {
		return false
	}
	// A leading `-` is only an indicator when a space follows it, and a
	// sequence entry is written `- -U`: the entry marker and the scalar are
	// separated by a space, so the scalar is `-U` and not a block. Healthcheck
	// arguments start with `-` constantly, and quoting every one of them makes
	// a check nobody can compare with the entry that declared it.
	if value[0] == '-' && (len(value) == 1 || value[1] == ' ') {
		return false
	}
	// A value that would parse as a number, a bool or null would not come back
	// out of the parser as the string the service was configured with.
	switch strings.ToLower(value) {
	case "true", "false", "yes", "no", "on", "off", "null", "~":
		return false
	}
	if _, err := strconv.ParseFloat(value, 64); err == nil {
		return false
	}
	return true
}
