# The smallest thing that can answer "did the build-time variable arrive".
#
# It exists because the assertion needs an image whose contents depend on a
# build argument, and app/ is not that: it is the product's own Dockerfile, and
# putting a test probe into it would ship the probe. This builds in seconds and
# is only ever built by CI.
#
# The value is written to a file rather than to a LABEL, because a label can
# only be read with a registry client and a file can be read by running the
# image — which the cluster can already do, and which proves the pull as well.
FROM busybox:1.37.0

ARG BUILD_PROBE=the-argument-never-arrived
RUN printf '%s' "$BUILD_PROBE" > /probe

# So the pod that reads it needs no shell of its own.
CMD ["cat", "/probe"]
