export GIT_COMMIT=git.$(subst -,.,$(shell git describe --always --match=NeVeRmAtCh --dirty 2>/dev/null || git rev-parse --short HEAD 2>/dev/null))

all: fmt clippy build-all

test-all:
	cargo test --release --workspace -- --nocapture

check: check-fmt check-clippy
fix: fmt clippy

build-all:
	cargo build --release

fmt:
	cargo fmt --all

check-fmt:
	cargo fmt --all -- --check

clippy:
	cargo clippy --release --fix --no-deps --tests

check-clippy:
	cargo clippy --release --no-deps --tests -- -D warnings
	# avoid missing checks like #![deny(missing_docs)]
	cargo clippy --release --no-deps -- -D warnings

dev-env:
	rustup component add clippy rustfmt
