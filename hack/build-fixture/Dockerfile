# Fixture for hack/build-e2e.sh (ADR 0021, pass B). It serves one line on
# port 8080, so the e2e can check the image it built and pushed.
#
# The RUN steps check the build network (ADR 0021, network isolation). Each
# probe is a service on the node's own address, which a RUN step must not
# reach: a page served by the e2e (PROBE_NODE), the Shoal API port
# (PROBE_API) and the portal port (PROBE_PORTAL). A connection refused or
# timed out is the expected result. PROBE_PUBLIC is on the internet, and a RUN
# step must reach it.
FROM busybox:1.37
ARG PROBE_NODE=
ARG PROBE_API=
ARG PROBE_PORTAL=
ARG PROBE_PUBLIC=
RUN test -n "$PROBE_NODE" && ! wget -q -T 5 -O /dev/null "$PROBE_NODE"
RUN test -n "$PROBE_API" && sh -c 'wget -q -T 5 -O /dev/null "$PROBE_API" 2>/tmp/err; grep -q "can.t connect" /tmp/err'
RUN test -n "$PROBE_PORTAL" && sh -c 'wget -q -T 5 -O /dev/null "$PROBE_PORTAL" 2>/tmp/err; grep -q "can.t connect" /tmp/err'
RUN test -n "$PROBE_PUBLIC" && wget -q -T 60 -O /dev/null "$PROBE_PUBLIC"
RUN echo "shoal build fixture ok" > /index.html
CMD ["httpd", "-f", "-p", "8080", "-h", "/"]
