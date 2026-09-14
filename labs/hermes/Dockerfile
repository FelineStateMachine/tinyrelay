# syntax=docker/dockerfile:1
FROM golang:1.27.1-bookworm AS tiny-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY protocol ./protocol
COPY tinyrelay ./tinyrelay
COPY tinygit ./tinygit
COPY tinyclient ./tinyclient
COPY tinyagent/*.go ./tinyagent/
COPY tinyagent/client ./tinyagent/client
RUN CGO_ENABLED=0 go build -mod=readonly -trimpath -o /out/tiny ./cmd/tiny && \
    CGO_ENABLED=0 go build -mod=readonly -trimpath -o /out/tinyagent ./tinyagent

FROM ghcr.io/astral-sh/uv:0.11.6-python3.13-trixie AS hermes-base
ARG HERMES_REVISION=1ad89ac018f26a4f21817ebf37bb09f508656d63
ARG HERMES_ARCHIVE_SHA256=388837b0bacb37381aafeb018543accc398f9c3e88931859f8f7a35c999d4a7c
RUN apt-get update && apt-get install --no-install-recommends -y ca-certificates curl git && \
    rm -rf /var/lib/apt/lists/*
WORKDIR /opt/hermes
RUN curl --fail --location --retry 3 \
      "https://codeload.github.com/NousResearch/hermes-agent/tar.gz/${HERMES_REVISION}" -o /tmp/hermes.tar.gz && \
    printf '%s  /tmp/hermes.tar.gz\n' "$HERMES_ARCHIVE_SHA256" | sha256sum -c - && \
    tar -xzf /tmp/hermes.tar.gz --strip-components=1 -C /opt/hermes && \
    rm /tmp/hermes.tar.gz && \
    printf '%s\n' "$HERMES_REVISION" > /opt/hermes-revision
COPY labs/hermes/source_check.py /lab/source_check.py
RUN python /lab/source_check.py record && \
    uv sync --frozen --no-dev --no-install-project --python /usr/local/bin/python3.13 && \
    python /lab/source_check.py verify
ENV PATH="/opt/hermes/.venv/bin:$PATH" \
    PYTHONPATH=/opt/hermes \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    HERMES_HOME=/home/hermes/.hermes
RUN useradd --create-home --uid 10001 hermes

FROM hermes-base AS hermes
COPY --from=tiny-build /out/tinyagent /usr/local/bin/tinyagent
COPY tinyagent/hermes /opt/tinyagent
COPY labs/hermes /lab
USER hermes
ENTRYPOINT ["python", "/lab/start_hermes.py"]

FROM hermes AS adapter-test
USER root
RUN uv pip install --python /opt/hermes/.venv/bin/python pytest==9.1.1 pytest-asyncio==1.3.0
USER hermes
ENV HERMES_HOME=/tmp/hermes-test
ENTRYPOINT ["python", "-m", "pytest", "-q", "-p", "no:cacheprovider", "--tb=short", "/opt/tinyagent/tests"]

FROM python:3.13-slim-bookworm AS tools
COPY --from=tiny-build /out/tinyagent /usr/local/bin/tinyagent
COPY labs/hermes /lab
ENV PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1
ENTRYPOINT ["python"]

FROM python:3.13-slim-bookworm AS tiny
COPY --from=tiny-build /out/tiny /usr/local/bin/tiny
COPY labs/hermes /lab
RUN useradd --create-home --uid 10001 relay && mkdir /data && chown relay /data
USER relay
ENTRYPOINT ["python", "/lab/start_tiny.py"]

FROM python:3.13-slim-bookworm AS model
COPY labs/hermes/model.py /lab/model.py
USER 10001:10001
ENTRYPOINT ["python", "/lab/model.py"]
