# testdata — the two files that are one contract

`config/kamal-backup.yml` says what a service backs up. `config/deploy.yml`'s
`backup` accessory carries the `env.secret` list and the `files:` mount, and
kamal-backup builds that accessory's environment **from that list and from
nothing else**. So a secret named in one file and missing from the other is two
files that each parse, each read correctly in review, and a pair that deploys and
then fails validation.

`caf backup` refuses that pair before it boots anything. The fixtures here are
what it refuses against.

## `identity/` — the real files, byte-identical

`kamal-backup.yml` and `deploy.yml` are byte-for-byte copies of identity's, at
identity `a20be0f` (`Merge identity-23: identity had no backup configuration`).
They are copies rather than synthetic fixtures because the property under test is
a property of a real file: the comments carry the reasons, and a hand-written
minimal fixture would agree with caf by construction — which is how a reader
that reads the wrong key passes every case it was written for.

| file | sha256 |
|---|---|
| `kamal-backup.yml` | `38dc7a3aec7ac4f7db6d66111536f8761514ef55763586b75f37db52d2e3fbff` |
| `deploy.yml` | `26c3ecad9860d1e46da4ba2a3a9af91b7075a33d5395b612d09df8a77ba1e589` |

`deploy.resolved.yml` is what `kamal config` printed for that `deploy.yml` on
kamal 2.12.0, with `KIT_SERVICE=identity`, `KIT_REGISTRY_ORG=cafaye`,
`KIT_REPO=identity`, `KIT_WEB_HOST=203.0.113.10`,
`KIT_APP_DOMAIN=id.example.invalid`, in a scratch git repository.

**It is captured output rather than assembled output, and that is the point.**
`kamal config` prints the top level with SYMBOL keys — `:accessories:` — because
it is `Kamal::Configuration#to_h`, a Ruby hash of symbols. The value of
`:accessories` is `raw_config.accessories`, so the inner keys are plain strings:
`image:`, `files:`, `env:`. A reader written against `:files` or `:env` finds
nothing, finds nothing *in the shape it expected*, and reports a fleet with no
backups as a fleet whose backups are fine.

`:version`, `:absolute_image` and `:service_with_version` carry the commit hash of
the scratch repository the render ran in. They are noise to this check and are
left exactly as measured rather than edited, so the file is what kamal printed.

## The other files are broken on purpose

Each one is the resolved document above with exactly one thing removed or
changed, and each names the violation it produces. They exist because a check
that has only ever been shown the good document has been shown that reading the
good document works — which is a weaker claim than reading a document correctly.

| file | what is wrong | the violation |
|---|---|---|
| `identity/deploy.resolved.secret-undeclared.yml` | `RESTIC_REPOSITORY` is gone from the accessory's `env.secret` | `secret-not-declared` |
| `identity/deploy.resolved.accessory-absent.yml` | the whole `backup` accessory is gone | `accessory-not-declared` |
| `identity/deploy.resolved.unmounted.yml` | the `files:` entry is gone | `config-not-mounted` |
| `identity/deploy.resolved.mount-rw.yml` | the mount is writable, not `ro` | `config-not-mounted` |

Every one of those four is a pair where **both files are internally consistent
and the pair is wrong** — the exact shape of the defect, which is why no per-file
parse can see it.

### The one that is not a defect, and is the harder half

The accessory declares **six** `env.secret` entries and
`kamal-backup.yml` names **four**:

| | |
|---|---|
| `kamal-backup.yml` names | `DATABASE_URL`, `DATABASE_PASSWORD`, `RESTIC_REPOSITORY`, `RESTIC_PASSWORD` |
| the accessory declares | those four, plus `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` |

The two extras are **not** a failure and not an oversight. restic reads those two
from the environment directly rather than through a `{ secret: }` in the config,
so nothing in the config has to name them — and the accessory has to declare them
or the S3 repository is unreachable. An extra entry costs the container one
variable; a missing one fails validation.

This is recorded here because it is the direction a caf gets wrong by accident.
The first version of the negative fixture removed `AWS_SECRET_ACCESS_KEY`, and
`Check` reported **zero** violations — correctly. The test that read that fixture
had asserted the check should fire, so the test reported the check as broken. The
test was wrong; the fixture has been corrected to remove `RESTIC_REPOSITORY`,
which the config does name. `TestAnAccessoryMayDeclareASecretTheConfigDoesNotUse`
now holds the other direction deliberately, so a caf that compares the two lists
as sets is caught rather than shipped.

## The negative fixtures in `kamal-backup.yml.negatives.yml`

One document, several defects, separated by `---`, so a reader can be shown each
without a directory per case. The index is positional: `config_test.go`'s
`negatives` table reads them in order, and the index and the bundle are asserted to
be the same length so a document added to one and not the other is a red test.

## What is NOT here, and why

No `paths:` fixture. identity's config has none, and the reason is written out at
the bottom of the file that has it: a backup of a database is not a backup of a
service, and identity writes nothing to disk to snapshot. A fixture with `paths:`
would test a second contract — the suspicious-path refusal, which is
kamal-backup's and not caf's — and adding it to this set would make the file that
is one contract a file that is two.
