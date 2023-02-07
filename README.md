A Reconciler to avoid crossplane keep polling the resource by adding the annotation `crossplane.io/paused`.

It will pause the resource if and only if:

1. The resource is Ready and Sync.
2. The time since last time we pause it is not longer than `FrozenTimeDuration`.
3. The resource is not deleted.

It will unpause the resource if one of the flowing condition is met:
1. The resource deleted.
2. The resource is paused longer than `UnPausePollInterval`.
3. The spec is updated.

See [example.go](cmd/example.go) about how to use it.

