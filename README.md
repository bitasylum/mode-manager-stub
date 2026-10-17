# mode-manager-stub

A test stand-in for the mode manager. It serves a web page with a mode select
box and writes the chosen mode to a `ClusterMode` object, so mode-operator can
be exercised end to end without the real mode manager.

**For test clusters only.** The page has no login. Reach it with
`kubectl port-forward`, and never expose it through an Ingress.

The design is in section 9.5 of the parent repo's
[design document](https://github.com/Audiobuzz/ship-core-mode-controller/blob/main/docs/design.md).

Status: not yet implemented.
