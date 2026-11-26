# TEST ONLY (specs/059-support-mailbox): the in-memory mail server of the support-mail workflow.
# The context is a directory holding the static binary:
#   CGO_ENABLED=0 go build -o /some/dir/mailsink ./dev/mailsink && docker build -f dev/mailsink/Dockerfile /some/dir
FROM gcr.io/distroless/static-debian12:nonroot
COPY mailsink /mailsink
USER 65532:65532
EXPOSE 2525 9993 8025
ENTRYPOINT ["/mailsink"]
