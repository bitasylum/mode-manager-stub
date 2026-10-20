# mode-manager-stub

A test stand-in for the mode manager. It writes a cluster's mode to
mode-operator's `default` ClusterMode, the way the mode manager does, and
serves a page for changing it, so mode-operator can be exercised end to end
without the real mode manager.

**For test clusters only.** The page has no login. Reach it with
`kubectl port-forward`, and never expose it through an Ingress.

It uses mode-operator's Go client library,
[github.com/bitasylum/mode-operator/client](https://github.com/bitasylum/mode-operator/tree/main/client),
for everything it does:

- **On startup** it decides the mode: it keeps the mode stored in the
  ClusterMode if any node has stayed up since it last wrote it, and otherwise,
  after a full cluster reboot, starts in the definition's default mode. It
  tells the two apart by the nodes' boot IDs, which it keeps in a ConfigMap
  and updates as nodes come and go. It records the decision as an event on the
  ClusterMode.
- **Before mode-operator's CRDs are installed** it holds the mode in memory,
  and writes it once they are.
- **Changes made by others**, a maintainer's `kubectl patch` or another
  program's write, become its current mode; it doesn't write them back.
- **The page** offers the modes the definition's transitions allow from the
  current mode, a move to any mode in as many steps as the transitions need,
  and a raw write that leaves the checking to the API server. It shows the
  ClusterMode's status, the startup decision and the fingerprint.

## Install

Install mode-operator first, or after. Its chart creates the role the stub is
bound to, `mode-operator-clustermode-writer` when its release is named
`mode-operator`, limited to the ClusterModes in its `modeManager.clusterModes`
value. That list must include the stub's `clusterMode`, which it does by
default; otherwise the stub's writes are refused as Forbidden.

```sh
kubectl create namespace mode-manager
kubectl label namespace mode-manager pod-security.kubernetes.io/enforce=restricted
helm install mode-manager-stub ./chart --namespace mode-manager \
  --set image.repository=<registry>/mode-manager-stub --set image.tag=<tag>
```

Build and push the image with
`make docker-build docker-push IMG=<registry>/mode-manager-stub:<tag>`.

| Value | Default | Meaning |
|---|---|---|
| `clusterMode`, `definition` | `default`, `standard` | the ClusterMode to write and the ClusterModeDefinition it follows |
| `writerLabel` | `mode-manager-stub` | the ClusterMode's `mode.bitasylum.org/writer` label |
| `builtinDefault` | `standby` | the mode to run in without the CRDs or the definition |
| `knownModes` | `[]` | the only modes to run in; a stored mode outside them is reset on startup. Empty means any |
| `discoveryInterval` | `1m` | how often to check for the CRDs while they aren't installed |
| `debounce` | `2s` | how long to wait after a node change before saving the fingerprint |
| `moveStepTimeout` | `10m` | how long a move waits for each mode to settle before it stops; `0s` means no limit |
| `modeOperator.writerClusterRole` | `mode-operator-clustermode-writer` | the role to bind, from mode-operator's chart; it must end in `clustermode-writer` |
| `networkPolicy.enabled`, `networkPolicy.from` | `true`, `[]` | a NetworkPolicy that lets only the peers in `from` connect; see [Use](#use) |
| `image.repository`, `image.tag`, `image.pullPolicy` | `ghcr.io/bitasylum/mode-manager-stub`, the chart's `appVersion`, `IfNotPresent` | the image |
| `imagePullSecrets`, `resources`, `podSecurityContext`, `securityContext`, `nodeSelector`, `tolerations`, `affinity` | | as usual for a Deployment; the security contexts meet the `restricted` Pod Security Standard |
| `service.port` | `8080` | the Service's port |
| `extraArgs` | `[]` | more flags, for example `--v=4`. Not `--namespace` or `--fingerprint-configmap`: the Role names the fingerprint ConfigMap the chart derives |

The fingerprint ConfigMap is `mode-manager-fingerprint-<clusterMode>`, in the
release's namespace.

The chart runs one replica with the `Recreate` strategy, so there is never
more than one writer. Besides the writer role, it grants list and watch on
Nodes; create, get and patch on its own fingerprint ConfigMap; and create and
patch on Events in the `default` namespace, where events about the
cluster-scoped ClusterMode are recorded.

`helm uninstall` leaves the ClusterMode and the fingerprint ConfigMap in
place, so a reinstall restores the mode as a restart would.

## Use

```sh
kubectl -n mode-manager port-forward svc/mode-manager-stub 8080
```

Then browse to <http://localhost:8080/>.

The page is also served through the API server's service proxy: run
`kubectl proxy`, then browse to
<http://localhost:8001/api/v1/namespaces/mode-manager/services/http:mode-manager-stub:http/proxy/>,
with the trailing slash.

The chart's NetworkPolicy keeps other pods out. It doesn't affect
port-forward, and it can't block the stub's own node, which kubelet's probes
need; on a single-node cluster that includes the API server, so the proxy
works as it is. On more nodes, list the addresses the API server connects
from in `networkPolicy.from`, for example the control-plane nodes'; which
addresses those are depends on the network plugin. Without a network plugin
that enforces NetworkPolicy, the policy has no effect.

The page uses a small JSON API, which scripts can use too:

| Request | Does |
|---|---|
| `GET api/state` | the stub's state: phase, current mode, the ClusterMode's status, the modes allowed next, the startup decision, the last adopted change, the last move, and the fingerprint |
| `POST api/mode` with `{"mode": "peak", "check": true}` | changes the mode; with `check`, only if the API server would accept it |
| `POST api/move` with `{"target": "maintenance"}` | moves to a mode in as many steps as the transitions need, in the background |
| `DELETE api/move` | stops the move; a change being written at that moment may still be made |

A change answers `{"ok": true}`, or `{"ok": false, "reason": ..., "message":
...}`, where `reason` is the client library's, for example
`TransitionNotAllowed` or `NotSettled`. Requests must be `application/json`
and come from the page's own origin, or from a program, which sends no
`Origin`. A change answers 409 while a move is running or the stub is
starting, a move answers 409 without the CRDs, and a stop answers 409 with
no move running. Without the CRDs, a mode
that isn't a valid mode name, or isn't in `knownModes`, answers 400.

## Development

```sh
make test        # unit tests
make lint
make vulncheck   # govulncheck, under the Go version the image is built with
make shellcheck
make helm-lint
make test-e2e    # the stub and mode-operator together on a new two-node kind cluster
```

`make test-e2e` builds mode-operator's image and installs its chart from a
checkout: `../mode-operator` if it exists, otherwise a clone of the commit
that go.mod's version of the client library names. Set `MODE_OPERATOR_DIR` to
use another. The tests use a kubeconfig of their own, so they leave your
current kubectl context alone, and they refuse to run if a kind cluster named
`mode-manager-stub-test-e2e` exists. `KEEP_CLUSTER=1` keeps the cluster
afterwards.

To build against a local checkout of the client library rather than the
version go.mod requires, run `make workspace`, which writes a `go.work` file
(ignored by git) that uses `../mode-operator/client`, or set `LIBRARY_PATH`.
`make docker-build` then builds the image against that checkout too, and
`make verify-published` builds and tests without it, as CI does. `make
docker-build` labels the image with the library it was built against, and
`make docker-push` refuses to push one built against a local checkout.

With other kind clusters running, `make test-e2e` needs
`fs.inotify.max_user_instances` of 512 or more, which it warns about:
`sudo sysctl fs.inotify.max_user_instances=512`.

## License

Apache 2.0. See [LICENSE](LICENSE).
