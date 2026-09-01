# Releasing

One tag publishes everything:

```bash
# 1. Bump the chart. version and appVersion must both equal the tag,
#    or the release workflow refuses to publish.
vim charts/presage/Chart.yaml

git commit -am "release: v0.2.0"
git tag v0.2.0
git push origin main v0.2.0
```

That produces:

| Artifact | Where |
| --- | --- |
| Controller image | `ghcr.io/breezycourses/presage:0.2.0` |
| Forecaster image | `ghcr.io/breezycourses/presage-forecaster:0.2.0` |
| Helm chart | `oci://ghcr.io/breezycourses/charts/presage:0.2.0` |
| Artifact Hub metadata | `ghcr.io/breezycourses/charts/presage:artifacthub.io` |

Images are built on native amd64 and arm64 runners and joined into a manifest
list. They are not cross-built under QEMU: the forecaster image installs torch,
and emulated arm64 turns a three-minute build into a forty-minute one.

## One-time setup

These are manual because they need account-level permissions a workflow token
does not have. **Until they are done, nothing is installable and the Artifact
Hub listing will not appear** — which is the state the project was in before
this document existed.

### 1. Make the packages public

A package pushed from a private repository is private, and `helm install`
against it fails with an authentication error rather than anything explanatory.
After the first release, for each of `presage`, `presage-forecaster`, and
`charts/presage`:

> github.com/orgs/breezycourses/packages → package → Package settings →
> Change visibility → Public

### 2. Add the repository to Artifact Hub

Sign in at [artifacthub.io](https://artifacthub.io), then Control Panel →
Repositories → Add, with:

| Field | Value |
| --- | --- |
| Kind | Helm charts |
| Name | `presage` |
| URL | `oci://ghcr.io/breezycourses/charts/presage` |

The URL **must** point at the chart itself, not at the namespace containing it.
`oci://ghcr.io/breezycourses/charts` will not index.

### 3. Claim ownership

Copy the repository ID Artifact Hub shows into `repositoryID` in
`artifacthub-repo.yml`, commit, and cut a release. The next run pushes the
metadata and the listing becomes verified.

## Checking a release

```bash
helm show chart oci://ghcr.io/breezycourses/charts/presage --version 0.2.0
docker buildx imagetools inspect ghcr.io/breezycourses/presage:0.2.0
oras manifest fetch ghcr.io/breezycourses/charts/presage:artifacthub.io
```

The last one is the check worth remembering: if it 404s, Artifact Hub has no
metadata for the repository no matter what the git tree says.

## Versioning

`v1alpha1` is pre-1.0 and the API may change between minor versions. The chart
version and the app version are kept identical — one project, one number — and
the release workflow enforces it.
