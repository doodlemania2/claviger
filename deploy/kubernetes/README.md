# Kubernetes

Plain manifests: a StatefulSet with one pod, a volume, a Service, and an optional Ingress.

## Why replicas must stay 1

Claviger stores data in SQLite. Two pods on one database corrupt it.
A `ReadWriteOnce` volume does not prevent this: it limits the volume to one *node*,
and two pods on that node can both mount it. Do not add an HPA. Do not scale up.

## Updates are effectively "recreate"

A StatefulSet stops the old pod, then starts the new one, one at a time. With one
replica that means a short outage on every update or node drain. That is the safe
behaviour here. There is no zero-downtime rollout for a single SQLite writer.

The image's entrypoint also has a handover step for platforms that start the new
replica first. On Kubernetes it is a safety net only. `PB_INSTANCE_ID` is set to the
pod name for it.

## Security context

The image runs as **root** (no `USER` in the Dockerfile). The manifest therefore does
not set `runAsNonRoot`, and does not pretend to. It does set a read-only root file system
(the entrypoint writes only to `/pb_data` and `/tmp`), drops all capabilities and
blocks privilege escalation. Running as non-root needs an image change.

## Prerequisites

- A cluster with a default StorageClass that gives block storage
  (`ReadWriteOnce`). Do not put `/pb_data` on NFS unless you know it supports POSIX
  byte-range locks (SQLite needs them), and never on SMB.
- For the Ingress: an ingress controller and cert-manager (or your own TLS secret).

## Deploy

```sh
cd deploy/kubernetes
# 1. Edit the image tag, and the Secret values in claviger.yaml.
#    Or leave the Secret out and create it yourself:
kubectl create namespace claviger
kubectl -n claviger create secret generic claviger \
  --from-literal=PB_ENCRYPTION_KEY="$(openssl rand -hex 16)" \
  --from-literal=PB_ADMIN_EMAIL=admin@example.com \
  --from-literal=PB_ADMIN_PASSWORD='a-long-random-password'
# (then delete the Secret block from claviger.yaml)

kubectl apply -f claviger.yaml
kubectl -n claviger rollout status statefulset/claviger

# 2. HTTPS and a stable name. Edit the host first.
kubectl apply -f ingress.yaml
```

Save `PB_ENCRYPTION_KEY` somewhere safe. Without it, encrypted settings cannot be read.
Do not commit the real Secret to git.

Passkeys need HTTPS and a host name that never changes (the "RP ID").

Quick test without Ingress:

```sh
kubectl -n claviger port-forward svc/claviger 8090:80
curl http://127.0.0.1:8090/api/health
```

## Real client IPs

Behind an Ingress, Claviger sees the controller's address. Set the Ingress
controller to pass `X-Forwarded-For`, then in the dashboard open Settings,
Application, "User IP proxy headers" and add `X-Forwarded-For`. If there is more
than one proxy in front (for example a cloud load balancer), also check "Use
leftmost IP". Only trust the header if all traffic passes through your proxy.

## Back up

Use CSI volume snapshots if your driver supports them:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata: { name: claviger-backup, namespace: claviger }
spec:
  source: { persistentVolumeClaimName: data-claviger-0 }
```

For point-in-time recovery, set the `LITESTREAM_*` variables in the Secret. The
image's built-in config targets Azure Blob Storage; for S3-compatible storage, mount
`../docker-compose/litestream.s3.yml` from a ConfigMap at `/etc/litestream.yml`
(use `subPath: litestream.yml`) and set `LITESTREAM_REPLICA_URL` to any non-empty
value, plus the `LITESTREAM_S3_*` variables. `LITESTREAM_*` is documented in
`../docker-compose/.env.example`. This was not tested on a cluster.

## Upgrade

```sh
# Edit the image tag in claviger.yaml, then:
kubectl apply -f claviger.yaml
kubectl -n claviger rollout status statefulset/claviger
kubectl -n claviger logs statefulset/claviger
```

Snapshot first, and read the release notes. A migration may not be reversible.

## Checked

`kubectl` could not validate these files, because it needs a cluster to look up the
API schema (even with `--dry-run=client`). The YAML parses, but the manifests were
not schema-checked or deployed. Run `kubectl apply --dry-run=server -f claviger.yaml`
against your cluster before you apply.
