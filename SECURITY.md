# Security policy

kubectl-topolvm only reads from the cluster: it gets and lists objects, and with `--metrics` or
`volumes --usage` reads the topolvm-node metrics and kubelet stats through the API server proxy.
It never changes or deletes anything and never reads Secrets. The one object it creates is a
SelfSubjectAccessReview for `rbac --check`, the same call as `kubectl auth can-i`, which the API
server answers without storing it.

## Reporting a vulnerability

Report it privately through GitHub: **Security → Report a vulnerability** on
<https://github.com/EsDmitrii/kubectl-topolvm>. Please do not open a public issue.

Only the latest release gets fixes.
