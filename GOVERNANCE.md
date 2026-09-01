# Governance

presage is a young project with a single maintainer. This document describes
how decisions are made today and how that is meant to grow, rather than
describing a structure the project does not yet have.

## Today

Decisions are made by the maintainers listed in [MAINTAINERS.md](MAINTAINERS.md).
In practice that is one person, so the process is: open an issue, discuss,
decide. Design discussions happen in the open on GitHub issues so the
reasoning is recoverable later.

Changes that alter the `v1alpha1` API, the default behaviour of the policy
engine, or the safety properties documented in the README should be proposed
as an issue before the pull request. Not for ceremony — those are the decisions
that are expensive to reverse once anyone has deployed them.

## Becoming a maintainer

Anyone who has contributed substantially and shown good judgement in review can
be invited to become a maintainer by an existing maintainer. "Substantially" is
deliberately not a commit count: sustained, high-quality review is worth more
than volume of code.

New maintainers are added by pull request to `MAINTAINERS.md`, and the
invitation is not a vote — while there is one maintainer there is nobody to
vote with. Once there are three or more, this section should be replaced with a
real process, and the project should adopt a lazy-consensus model for ordinary
changes with explicit agreement required for API and governance changes.

## Making decisions when maintainers disagree

Not yet applicable. When it becomes applicable, the intent is: prefer the
option that is easier to reverse, prefer the option that keeps a workload safe
over the one that saves money, and write down why.

## Project scope

presage forecasts demand and turns it into replica counts. Things that belong
here: forecasting backends, scale targets, and the decision layer between them.

Things that do not: a metrics store, a general-purpose ML serving platform, or
a replacement for HPA in cases where reactive scaling already works. If lead
time is negligible, presage has nothing to offer and should say so rather than
grow features to compete.
