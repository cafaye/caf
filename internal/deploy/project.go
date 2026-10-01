package deploy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The two files a deployment is made of, relative to the project directory.
const (
	configName  = "deploy.yml"
	secretsName = "secrets"
	secretsDir  = ".kamal"
)

// Read looks at a project directory and returns what a deploy needs to know
// about it, refusing with a message that names the file that is missing.
//
// It checks existence and nothing else. Whether a config is *valid* is
// Kamal's answer and not caf's: caf has no ERB interpreter, and a caf that
// guessed at a template would be a second opinion about a document it cannot
// read.
func Read(dir, service, env string) (Deployment, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Deployment{}, fmt.Errorf("resolve %s: %w", dir, err)
	}

	info, err := os.Stat(abs)
	if err != nil {
		return Deployment{}, fmt.Errorf("caf deploy: %s is not a directory caf can read: %w", abs, err)
	}
	if !info.IsDir() {
		return Deployment{}, fmt.Errorf("caf deploy: %s is not a directory", abs)
	}

	dep := Deployment{
		Dir:     abs,
		Service: service,
		Env:     env,
		Config:  filepath.Join(abs, "config", configName),
	}

	if !fileExists(dep.Config) {
		return Deployment{}, fmt.Errorf("caf deploy: %s has no %s, so there is nothing to deploy.\n"+
			"  Copy it from cafaye/kit at the ref your kit.ref names: kit/templates/kamal/deploy.yml.erb -> config/deploy.yml\n"+
			"  It is a Kamal configuration and needs the KIT_* variables in the shell that runs the deploy",
			abs, filepath.Join("config", configName))
	}

	if env != "" {
		dep.Overlay = filepath.Join(abs, "config", strings.TrimSuffix(configName, ".yml")+"."+env+".yml")
		if !fileExists(dep.Overlay) {
			return Deployment{}, fmt.Errorf("caf deploy: -env %s reads %s, which does not exist.\n"+
				"  Kamal merges config/deploy.yml with that overlay, so the environment is a file and not a flag on its own.\n"+
				"  Write it, or deploy without -env to use config/deploy.yml alone",
				env, dep.Overlay)
		}
	}

	for _, secrets := range secretFiles(env) {
		if fileExists(filepath.Join(abs, secrets)) {
			dep.Secrets = append(dep.Secrets, filepath.Join(abs, secrets))
		}
	}
	return dep, nil
}

// secretFiles are the paths Kamal reads for an environment, relative to the
// project, in Kamal's own order. An empty environment means no destination, and
// Kamal then reads the single default path.
//
// This is Kamal's rule, not caf's, and it is the one a caf that guessed would get
// wrong in the way that hurts: telling an operator their credentials are in
// .kamal/secrets when the environment they just asked for means Kamal will not
// open it.
func secretFiles(env string) []string {
	if env == "" {
		return []string{filepath.Join(secretsDir, secretsName)}
	}
	base := filepath.Join(secretsDir, secretsName)
	return []string{base + "-common", base + "." + env}
}

// WantedSecretFiles is every file Kamal would read for an environment, present or
// not. It is separate from Deployment.Secrets, which holds only the ones that
// exist: a report has to be able to name a file that is MISSING, or it can only
// say "there are none" when what it should say is "there is one, at this path".
func WantedSecretFiles(env string) []string { return secretFiles(env) }

// SecretValues reads the names and values out of a project's secrets file.
//
// It exists so a test can assert that no secret VALUE reaches any output, and
// nothing in the command calls it. A deploy never needs a secret's value; the
// only thing it needs is the file's existence, which Read already decided, and
// a function that can read credentials is a function one bug away from printing
// them.
func SecretValues(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if name == "" || value == "" {
			continue
		}
		values[name] = value
	}
	return values, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
