FROM rust:1.91-slim-bookworm AS build
RUN apt-get update && apt-get install -y --no-install-recommends pkg-config libssl-dev git && rm -rf /var/lib/apt/lists/*
WORKDIR /src
# Build dependencies first so source edits don't recompile actix and friends.
COPY Cargo.toml Cargo.lock ./
RUN mkdir src && echo 'fn main() {}' > src/main.rs && cargo build --release --locked && rm -rf src
COPY menu.json ./
COPY src ./src
RUN touch src/main.rs && cargo build --release --locked

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends libssl3 ca-certificates && rm -rf /var/lib/apt/lists/* \
	&& useradd --uid 65532 --no-create-home --shell /usr/sbin/nologin app
COPY --from=build /src/target/release/ussd-demo /usr/local/bin/ussd-demo
USER app
EXPOSE 8080
# ussdframework logs through the `log` facade and this service installs no
# logger, so nothing about a session (PINs included) is ever printed.
CMD ["/usr/local/bin/ussd-demo"]
