# A TCP forwarder standing in for the `embed` service, so the stack can talk to
# an embedder running on the macOS host instead of inside the podman VM.
#
# This exists as a Dockerfile rather than a bare `image:` in the override
# because the base compose file's `embed` service declares both `build:` and
# `image:`. An override that set only `image:` would leave the base `build:`
# in place, and `podman compose up --build` would then build engram-local and
# tag it with this service's image name. Overriding both keeps the pair
# coherent.
FROM docker.io/alpine/socat:latest
