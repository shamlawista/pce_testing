# How to Use the Docker Image

## Obtain the Docker Image

### Option 1: Use the Published Image

Pull the latest image from GHCR:

```bash
docker pull ghcr.io/nttcom/pola:latest
```

### Option 2: Build the Docker Image Locally

To build the Docker image locally, run the following command from the repository root (`pola/`, which contains `build/`):

```bash
docker buildx build \
    -t <image-name> \
    -f build/package/Dockerfile \
    --load \
    .
```

Note: To build the debug image, which adds a shell and network troubleshooting tools on top of the same binaries, use `Dockerfile.debug` instead:

```bash
docker buildx build \
    -t <image-name> \
    -f build/package/Dockerfile.debug \
    --load \
    .
```

### Option 3: Build with Make Targets

From the repository root, you can build images using Makefile targets:

```bash
# Build production image as pola:latest
make image

# Build debug image as pola:latest-debug
make image-debug
```

You can override image name and tag:

```bash
make image IMAGE=<image-name> TAG=<tag>
make image-debug IMAGE=<image-name> TAG=<tag>
```

## Run with Host Network Mode

`polad` reads `polad.yaml` from its working directory, which is `/pola` inside the container. Mount your config directory there as shown below.

```bash
# Prepare polad config directory for volume mount
mkdir -p pola-config

# Prepare polad log directory for volume mount
LOGDIR="$(pwd)/logs"
mkdir -p "$LOGDIR"

# Prepare the persistence directory for volume mount - shared by SR policy
# intent (global.intentPersistence) and the global node-exclusion set
# (global.nodeExclusionPersistence), both enabled by default; skip this if
# you've set `enable: false` for both in polad.yaml
INTENTDIR="$(pwd)/lib"
mkdir -p "$INTENTDIR"

# Create a polad configuration file
# Reference:
# https://github.com/nttcom/pola/blob/main/docs/sources/getting-started.md#configuration
vi "pola-config/polad.yaml"

# Start the container
docker run -d --network host \
    -v "$(pwd)/pola-config:/pola" \
    -v "$LOGDIR:/var/log/pola" \
    -v "$INTENTDIR:/var/lib/pola" \
    ghcr.io/nttcom/pola:latest
```

`global.intentPersistence` (path `/var/lib/pola/intents.json`) and `global.nodeExclusionPersistence` (path `/var/lib/pola/node-exclusions.json`) are both enabled by default and share this same directory, so the one mount above covers both - needed for them to actually survive a container recreation, exactly the scenario those features exist for. Without it, `polad` still starts fine (a missing/unwritable directory only disables the affected feature for that run, logged as a warning, never fatal), it just won't remember SR policy intent or the global node-exclusion set across restarts. Skip the mount if you've explicitly disabled both.

## Run with Bridge Network Mode

`polad` reads `polad.yaml` from its working directory, which is `/pola` inside the container. Mount your config directory there as shown below.

```bash
# Create a dedicated network for PCEP communication
docker network create --subnet <PCEP network subnet> pcep_net

# Prepare polad config directory for volume mount
mkdir -p pola-config

# Prepare polad log directory for volume mount
LOGDIR="$(pwd)/logs"
mkdir -p "$LOGDIR"

# Prepare the persistence directory for volume mount - shared by SR policy
# intent (global.intentPersistence) and the global node-exclusion set
# (global.nodeExclusionPersistence), both enabled by default; skip this if
# you've set `enable: false` for both in polad.yaml
INTENTDIR="$(pwd)/lib"
mkdir -p "$INTENTDIR"

# Create a polad configuration file
# Reference:
# https://github.com/nttcom/pola/blob/main/docs/sources/getting-started.md#configuration
vi "pola-config/polad.yaml"

# Start the container
docker run -d --network pcep_net --ip <PCE Address> \
    -v "$(pwd)/pola-config:/pola" \
    -v "$LOGDIR:/var/log/pola" \
    -v "$INTENTDIR:/var/lib/pola" \
    ghcr.io/nttcom/pola:latest

# Connect the PCC container to the network
docker network connect pcep_net <PCC container name>
```

`global.intentPersistence` (path `/var/lib/pola/intents.json`) and `global.nodeExclusionPersistence` (path `/var/lib/pola/node-exclusions.json`) are both enabled by default and share this same directory, so the one mount above covers both - needed for them to actually survive a container recreation, exactly the scenario those features exist for. Without it, `polad` still starts fine (a missing/unwritable directory only disables the affected feature for that run, logged as a warning, never fatal), it just won't remember SR policy intent or the global node-exclusion set across restarts. Skip the mount if you've explicitly disabled both.
