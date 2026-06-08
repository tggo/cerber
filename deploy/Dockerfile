# cerber — production proxy image (NO TLS impersonation).
#
# Built on the target host from a prebuilt linux/amd64 binary (`make deploy`
# cross-compiles it), so this is just a COPY into a distroless base — no Go
# toolchain, no qemu, no network fetch of source. The static base ships CA
# certificates so cerber can reach provider APIs over HTTPS.
FROM gcr.io/distroless/static-debian12:nonroot
COPY cerber /usr/local/bin/cerber
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cerber"]
