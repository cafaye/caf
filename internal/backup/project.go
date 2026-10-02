package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The files a backup is made of, relative to the project directory.
const (
	configName       = "deploy.yml"
	backupConfigName = "kamal-backup.yml"
	secretsDir       = ".kamal"
	secretsKey       = "secrets"
)

// Cover is everything caf knows about a service's backup before it asks Kamal
// anything: where the project is, which Kamal config describes it, and where the
// credentials come from.
//
// It is a fact about files that exist, not about a configuration that has been
// resolved — resolving one is Kamal's job and needs Kamal, because
// `config/deploy.yml` is an ERB template. So nothing here is validated for
// meaning: existence is decided here, the backup configuration is read by
// ReadConfig, and the contract between the two files is decided in contract.go
// against Kamal's own resolved output.
type Cover struct {
	// Dir is the absolute project directory. Everything caf names to the operator
	// is absolute, so a message is unambiguous about which project it is about.
	Dir string
	// Service is the service being backed up. It came from the manifest, not from
	// the Kamal config: Kamal's `service:` is the container name and the two are
	// not required to agree.
	Service string
	// Env is the Kamal destination — the environment overlay. Empty means no
	// overlay, which is a different thing from an overlay named "default".
	Env string
	// Config is config/deploy.yml, the base document every environment inherits.
	Config string
	// Overlay is config/deploy.<env>.yml, or "" when the environment has none.
	Overlay string
	// Secrets are the files Kamal will actually read, in the order it reads them.
	//
	// The list is longer than one for a reason worth stating: naming a
	// destination changes where credentials come from. Kamal's
	// Kamal::Secrets#secrets_filenames is
	//
	//	["<path>-common", "<path>.<destination>"]
	//
	// so `--destination staging` reads .kamal/secrets-common and
	// .kamal/secrets.staging and does NOT read .kamal/secrets. Measured on kamal
	// 2.12.0. So caf resolves and names them rather than assuming the one
	// `kamal init` creates.
	Secrets []string
}

// Read looks at a project directory and returns what a backup needs to know
// about it, refusing with a message that names the file that is missing.
//
// It checks existence and nothing else. Whether `config/deploy.yml` is *valid*
// is Kamal's answer and not caf's, whether `config/kamal-backup.yml` says
// anything useful is ReadConfig's, and whether the two agree is contract.go's.
func Read(dir, service, env string) (Cover, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Cover{}, fmt.Errorf("resolve %s: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Cover{}, fmt.Errorf("caf backup: %s is not a directory caf can read: %w", abs, err)
	}
	if !info.IsDir() {
		return Cover{}, fmt.Errorf("caf backup: %s is not a directory", abs)
	}

	cover := Cover{
		Dir:     abs,
		Service: service,
		Env:     env,
		Config:  filepath.Join(abs, "config", configName),
	}

	if !isFile(cover.Config) {
		return Cover{}, fmt.Errorf("caf backup: %s has no %s, so there is no deployment to back up.\n"+
			"  Copy it from cafaye/kit: templates/kamal/deploy.yml.erb -> config/deploy.yml\n"+
			"  Deploy the service with `caf deploy` first. A backup accessory is an accessory: with no deploy config "+
			"there is nothing for it to be an accessory of, and config/kamal-backup.yml would be a file nothing reads",
			abs, filepath.Join("config", configName))
	}

	if env != "" {
		cover.Overlay = filepath.Join(abs, "config", strings.TrimSuffix(configName, ".yml")+"."+env+".yml")
		if !isFile(cover.Overlay) {
			return Cover{}, fmt.Errorf("caf backup: -env %s reads %s, which does not exist.\n"+
				"  Kamal merges config/deploy.yml with that overlay, so the environment is a file and not a flag on its own.\n"+
				"  Write it, or back up without -env to use config/deploy.yml alone",
				env, cover.Overlay)
		}
	}

	for _, rel := range secretFiles(env) {
		if isFile(filepath.Join(abs, rel)) {
			cover.Secrets = append(cover.Secrets, filepath.Join(abs, rel))
		}
	}
	return cover, nil
}

// secretFiles are the paths Kamal reads for an environment, relative to the
// project, in Kamal's own order. An empty environment means no destination, and
// Kamal then reads the single default path.
//
// This is Kamal's rule, not caf's, and it is the one a caf that guessed would
// get wrong in the way that hurts: telling an operator their credentials are in
// .kamal/secrets when the environment they just asked for means Kamal will not
// open it.
func secretFiles(env string) []string {
	if env == "" {
		return []string{filepath.Join(secretsDir, secretsKey)}
	}
	base := filepath.Join(secretsDir, secretsKey)
	return []string{base + "-common", base + "." + env}
}

// WantedSecretFiles is every file Kamal would read for an environment, present or
// not. It is separate from Cover.Secrets, which holds only the ones that exist:
// a report has to be able to name a file that is MISSING, or it can only say
// "there are none" when what it should say is "there is one, at this path".
func WantedSecretFiles(env string) []string { return secretFiles(env) }

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
