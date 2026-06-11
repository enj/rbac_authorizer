# monis.app/kk/rbac_authorizer

the Kubernetes RBAC authorizer as an independently consumable Go module.

## What this is

This module is generated. Its code is copied from k8s.io/kubernetes and
modified so that it can be consumed as an ordinary Go module, which the
upstream repository cannot be: upstream requires its own staging modules
through repository local directory replacements that resolve nowhere else.

It is not a Kubernetes release. It is not produced by, endorsed by, or
affiliated with the Kubernetes project. See NOTICE for the full attribution,
licence, and trademark statement, and for the record of what was changed.

## Provenance

| field | value |
| --- | --- |
| upstream module | `k8s.io/kubernetes` |
| upstream repository | `https://github.com/kubernetes/kubernetes.git` |
| upstream commit | `24e2b02af5543d7910c2bb074c7264df5a8f0467` |
| upstream release | `v1.36.2` |
| relocated below | `internal/kk` |

## Public API

Package `rbacauthorizer` at the module root is the entire public API. Every
name below is an alias of, or forwards to, the relocated upstream declaration
it was generated from, so a value it produces is the upstream value and an
implementation of an interface it publishes satisfies the upstream contract.

- `Attributes`
- `AttributesRecord`
- `AuthorizationRuleResolver`
- `Authorizer`
- `ClusterRoleBindingLister`
- `ClusterRoleBindingListerFromLister`
- `ClusterRoleGetter`
- `ClusterRoleGetterFromLister`
- `Decision`
- `DecisionAllow`
- `DecisionDeny`
- `DecisionNoOpinion`
- `DefaultNonResourceRuleInfo`
- `DefaultResourceRuleInfo`
- `DefaultRuleResolver`
- `DefaultUserInfo`
- `New`
- `NewDefaultRuleResolver`
- `NewSubjectAccessEvaluator`
- `NonResourceRuleInfo`
- `RBACAuthorizer`
- `RequestToRuleMapper`
- `ResourceRuleInfo`
- `RoleBindingLister`
- `RoleBindingListerFromLister`
- `RoleGetter`
- `RoleGetterFromLister`
- `RoleToRuleMapper`
- `RuleAllows`
- `RuleResolver`
- `RulesAllow`
- `SubjectAccessEvaluator`
- `SubjectLocator`
- `UserInfo`

## Usage

```go
import "monis.app/kk/rbac_authorizer"
```

## What you may depend on

Only package `rbacauthorizer`. Everything under `internal/kk` is relocated
upstream code, it is reorganised whenever upstream changes, and Go will refuse
to let you import it from another module in any case.

Each relocated package carries a `SOAPBOX_PROVENANCE.txt` file naming its
upstream path, the upstream commit it was taken at, and every change made to
it. Each modified file carries a notice above its package clause saying that
it is not the upstream original.

## Licence

The copied code keeps its upstream licence, reproduced unchanged in `LICENSE`.
`NOTICE` carries the upstream attribution notices in full, together with the
statement of modification the licence requires.
