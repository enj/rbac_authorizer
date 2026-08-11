A github template repo at enj/soapbox

The idea is to mimic https://github.com/kubernetes/kubernetes staging directory concept

But for any folder or set of folders in k/k (so that another repo can depend on the code without having to importing all of k/k)

When cloned, the template would take the folders and any patches as config, and then use a github action to generate (and keep updated) the code from those folders.  Tags should match k/k and the original
git SHAs should be included in the git commit messages.

my domain is monis.app (ideally this would be configurable)

I want go imports to be at monis.app/kk/...

It may be possible to use the existing staging repo code as a base.

All code should be Go (no shell, use go run as needed)

Say I want to use https://github.com/kubernetes/kubernetes/tree/master/plugin/pkg/auth/authorizer/rbac as monis.app/kk/rbac_authorizer

The github action would copy the relevant code and rewrite the imports as needed to make it work.

Patches need to be supported to make it possible to export internals if needed.

Once set up there should be no maintenance outside of possible conflict resolution fixes

Ideally I would be able to control the public Go API that is exposed, the simplest approach to that might be to actually stick all code inside of an `internal` package and then only expose the parts we want others to consume at the outer layer.

All commits in enj/soapbox should be

- SSH signed with ~/.config/wunderkind/ssh/id_ed25519.pub
- author + committer set to Monis Khan <i@monis.app>
- have this trailer: Signed-off-by: Monis Khan <mok@microsoft.com>

All commits in the generate staging repos should match the upstream commit as close as possible in terms of author/message/etc.
