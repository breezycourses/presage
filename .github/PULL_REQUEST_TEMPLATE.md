## What this changes

<!-- The diff shows what. Explain why. -->

## Why

## What this does not do

<!-- Naming your own gaps makes a PR easier to trust than implying it is
     complete. Delete this section only if it is genuinely empty. -->

## Testing

<!-- Which layer covers this, and why that layer? See CONTRIBUTING.md.
     - unit           `make test`
     - controller e2e `make test-e2e`      (touches the API server)
     - model e2e      `make test-model-e2e` (touches TimesFM)
     - chart          `make chart-validate` -->

## Checklist

- [ ] `make verify` passes
- [ ] `make test-e2e` passes, or this does not touch the controller
- [ ] `make generate && make chart-sync-crds` run and committed, or the API is unchanged
- [ ] Safety-relevant changes (reactive floor, rate limits, `maxReplicas`, Agones fallback) have a test that fails without them
