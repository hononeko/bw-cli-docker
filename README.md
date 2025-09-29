# Bitwarden CLI Proxy & Webhook

This container image runs the [Bitwarden CLI](https://github.com/bitwarden/clients) (`bw`) and exposes its `serve` functionality through a proxy on port 8087. The proxy provides endpoints for health checks and manual synchronization, and automatically runs a periodic background sync to keep the vault up-to-date.

It is designed to be used as a webhook provider for the [External Secrets Operator](https://external-secrets.io/latest/) (ESO) in Kubernetes, allowing you to sync secrets from a self-hosted Vaultwarden or Bitwarden instance.

The entrypoint is a small Go program that handles the initial login and unlock flow, starts the `bw serve` process, runs a periodic sync task, and exposes everything through a reverse proxy.

## ✨ Security Highlights

This image is built with security as a top priority, adhering to modern best practices for containerization.

**Distroless**: The final image is built on a Google distroless base. It contains only the application and its runtime
dependencies. It does not include a shell, package manager, or other standard utilities, which dramatically reduces the
attack surface and eliminates a huge number of potential CVEs.

**Rootless**: The container process runs as a non-privileged nonroot user by default. This prevents a whole class of
container-breakout vulnerabilities and ensures that even if the application were compromised, the attacker would not
have root access within the container.

## 🚀 Usage

This image is intended to be run as a service inside a Kubernetes cluster.

### Kubernetes Manifest
Here is an example Deployment that uses this image and exposes it as a service for ESO.

```YAML
apiVersion: apps/v1
kind: Deployment
metadata:
    name: bitwarden-cli
    namespace: external-secrets # Or your preferred namespace
    labels:
        app: bitwarden-cli
spec:
    replicas: 1
    selector:
        matchLabels:
            app: bitwarden-cli
    template:
        metadata:
            labels:
                app: bitwarden-cli
        spec:
            containers:
                - name: bitwarden-cli
                  image: ghcr.io/vehkiya/bw-cli:latest # 👈 Image here
                  ports:
                      - containerPort: 8087
                  env:
                      - name: BW_HOST
                        value: "https://vaultwarden.your.domain" # Optional
                      - name: BW_CLIENTID
                        valueFrom:
                            secretKeyRef:
                                name: vaultwarden-credentials
                                key: BW_CLIENTID
                      - name: BW_CLIENTSECRET
                        valueFrom:
                            secretKeyRef:
                                name: vaultwarden-credentials
                                key: BW_CLIENTSECRET
                      - name: BW_PASSWORD
                        valueFrom:
                            secretKeyRef:
                                name: vaultwarden-credentials
                                key: BW_PASSWORD
                      - name: BW_SYNC_INTERVAL
                        value: "15m" # Optional: defaults to 2m
                      - name: BW_DISABLE_SYNC
                        value: "false" # Optional: disables automatic sync
                  readinessProbe:
                    httpGet:
                      path: /healthz
                      port: 8087
                    initialDelaySeconds: 5
                    periodSeconds: 10
                  livenessProbe:
                    httpGet:
                      path: /healthz
                      port: 8087
                    initialDelaySeconds: 15
                    periodSeconds: 20
---
apiVersion: v1
kind: Service
metadata:
    name: bitwarden-cli-service
    namespace: external-secrets
spec:
    selector:
        app: bitwarden-cli
    ports:
        - protocol: TCP
          port: 8087
          targetPort: 8087
```

### API Endpoints

The proxy server provides the following endpoints:

#### `GET /healthz`

A simple health check endpoint. It returns a `200 OK` status if the proxy server is running. This is suitable for use in Kubernetes liveness and readiness probes.

#### `POST /sync`

This endpoint triggers a `bw sync` command to manually synchronize the vault with the Bitwarden server. This is useful to force an update after making changes to your vault. This endpoint is also called automatically in the background on a periodic basis.

#### `/*`

All other requests are proxied directly to the `bw serve` process. This is how the External Secrets Operator will interact with the Bitwarden vault.

### Alternative Sync Methods

If you disable the built-in periodic sync (`BW_DISABLE_SYNC: "true"`), you can still trigger synchronization externally. This is useful if you prefer to manage synchronization on your own schedule.

A common approach in Kubernetes is to use a `CronJob` to call the `/sync` endpoint. Here is an example that runs every 15 minutes:

```YAML
apiVersion: batch/v1
kind: CronJob
metadata:
  name: bitwarden-sync-cron
  namespace: external-secrets # Match the namespace of your service
spec:
  schedule: "*/15 * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: bitwarden-sync-trigger
            image: curlimages/curl:latest
            args:
            - -X
            - POST
            - http://bitwarden-cli-service:8087/sync
          restartPolicy: OnFailure
```

## 🔧 Environment Variables

The container is configured using the following environment variables.

| Variable           | Description                                                                    | Required | Default |
|--------------------|--------------------------------------------------------------------------------|----------|---------|
| BW_HOST            | The full URL of your Vaultwarden/Bitwarden instance.                           | No       | `N/A`   |
| BW_CLIENTID        | The API Key Client ID from your Bitwarden account.                             | Yes      | `N/A`   |
| BW_CLIENTSECRET    | The API Key Client Secret from your Bitwarden account.                         | Yes      | `N/A`   |
| BW_PASSWORD        | Your master password, used to unlock the vault.                                | Yes      | `N/A`   |
| BW_SYNC_INTERVAL   | The interval for periodic background syncs (e.g., `2m`, `1h`, `15m`).           | No       | `2m`    |
| BW_DISABLE_SYNC    | Disables automatic background sync when set to `true`.                         | No       | `false` |

## 🛠️ Building the Image

To build the image locally, use the provided Dockerfile.

```Bash
# Build and tag the image
docker build -t ghcr.io/<your_github_user>/bw-cli:latest .

# Push the image to the GitHub Container Registry
docker push ghcr.io/<your_github_user>/bw-cli:latest
```