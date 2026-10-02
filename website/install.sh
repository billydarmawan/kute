#!/bin/sh
set -eu

repo="kute-dev/kute"
bin="kute"
verify_docs="https://kute.dev/verify.html"

# The keyless signing identity is the release workflow itself. Matched by
# regexp rather than by exact tag so this script never needs to know which
# version it just fetched; the anchor is what keeps it to that one workflow
# in that one repo.
cert_identity="^https://github\.com/${repo}/\.github/workflows/release\.yml@refs/tags/"
cert_issuer="https://token.actions.githubusercontent.com"

fail() {
	printf 'kute install: %s\n' "$1" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "missing required command: $1"
}

cleanup() {
	[ -n "${tmp:-}" ] && rm -rf "$tmp"
}

# The checksum verified below proves the archive matches a manifest that came
# down the same wire as the archive; only the signature says the release is
# ours. cosign is not a dependency of this script, so a machine without it
# gets a note and the checksum — a stronger check that nobody can run is not
# stronger. Same for a release predating signing (before v0.4.0-beta.1):
# don't fail an install that used to work — but only those.
verify_signature() {
	if ! command -v cosign >/dev/null 2>&1; then
		printf 'note: cosign not found; verified checksum only.\n'
		printf '      %s explains how to check the signature.\n' "$verify_docs"
		return 0
	fi

	bundle="${tmp}/${archive}.sigstore.json"
	if ! curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "$bundle" "${base_url}/${archive}.sigstore.json" 2>/dev/null; then
		# Only the releases that predate signing may lack a bundle. From
		# v0.4.0-beta.1 on, a missing one is exactly what someone who
		# swapped the archive and its checksum manifest would also arrange,
		# so with cosign present it fails rather than quietly downgrading.
		case "$version" in
		v0.1.* | v0.2.* | v0.3.*)
			printf 'note: %s predates release signing; verified checksum only.\n' "$version"
			return 0
			;;
		esac
		fail "could not download the signature for ${archive} (every release from v0.4.0-beta.1 is signed) — do not run it; see ${verify_docs}"
	fi

	# cosign 3 reads the bundle format by default and has no
	# --new-bundle-format flag; cosign 2 needs it and rejects a bundle
	# without it. Rather than parse `cosign version`, try the modern form and
	# fall back — a bundle that is genuinely bad fails both ways, so the
	# retry can only rescue an old cosign, never hide a bad signature.
	# verify_blob_with reports 0 verified, 1 rejected (a verdict cosign
	# actually reached) and 2 unverifiable (cosign never got to judge the
	# bundle); only the rejected status means "do not run it". The else
	# clauses capture the failure status: `if` with a false condition and no
	# else exits 0, so it cannot be read after the statement.
	if verify_blob_with "$bundle"; then
		printf 'Verified signature (cosign, keyless).\n'
		return 0
	else
		st1=$?
	fi
	if verify_blob_with "$bundle" --new-bundle-format; then
		printf 'Verified signature (cosign, keyless).\n'
		return 0
	else
		st2=$?
	fi
	if [ "$st1" -eq 2 ] || [ "$st2" -eq 2 ]; then
		printf 'note: cosign could not verify — Sigstore trust bootstrap failed; verified checksum only.\n'
		printf '      %s explains how to check the signature.\n' "$verify_docs"
		return 0
	fi

	fail "signature verification failed for ${archive} — do not run it; see ${verify_docs}"
}

# Exit status is the signal: 0 verified, 1 rejected (cosign judged the
# bundle and said no), 2 unverifiable (cosign never judged it).
verify_blob_with() {
	bundle="$1"
	shift
	# stdout is discarded — "Verified OK" is the only thing cosign puts there
	# and it means nothing to a machine. stderr is kept because the error
	# text is what separates an infrastructure failure from a verdict.
	if err="$(cosign verify-blob \
		--certificate-identity-regexp "$cert_identity" \
		--certificate-oidc-issuer "$cert_issuer" \
		--bundle "$bundle" \
		"$@" \
		"${tmp}/${archive}" 2>&1 >/dev/null)"; then
		return 0
	fi
	# cosign bootstraps Sigstore's TUF trust root before it looks at a byte
	# of the bundle. A failed bootstrap — the tuf-repo-cdn.sigstore.dev
	# 403/404 outage, a DNS failure, a blocked proxy — exits non-zero
	# without ever judging the signature. That is the cosign-not-found case
	# with cosign present, and deserves the same fallback, not "do not run
	# it". A bundle cosign genuinely rejected fails after bootstrap with a
	# different message and hard-fails below.
	case "$err" in
		*"setting trusted material"*|*"getting trusted root"*|*"failed to create TUF client"*|*"tuf refresh failed"*)
			return 2 ;;
	esac
	return 1
}

main() {
	version="${KUTE_VERSION:-latest}"
	install_dir="${KUTE_INSTALL_DIR:-/usr/local/bin}"

	need curl
	need tar

	case "$(uname -s)" in
		Linux) os="linux" ;;
		Darwin) os="darwin" ;;
		# Git Bash, MSYS2 and Cygwin are where a Windows user lands after
		# copying the curl one-liner off the website. There is a Windows
		# build; it just isn't installed from here.
		MINGW*|MSYS*|CYGWIN*)
			fail "Windows is not installed from this script. Use PowerShell:
    irm https://kute.dev/install.ps1 | iex
  or Scoop:
    scoop bucket add kute-dev https://github.com/kute-dev/scoop-bucket
    scoop install kute"
			;;
		*) fail "unsupported OS: $(uname -s)" ;;
	esac

	case "$(uname -m)" in
		x86_64|amd64) arch="amd64" ;;
		arm64|aarch64) arch="arm64" ;;
		*) fail "unsupported architecture: $(uname -m)" ;;
	esac

	# uname -m lies under Rosetta; ask the kernel if we are translated.
	if [ "$os" = "darwin" ] && [ "$arch" = "amd64" ] &&
		[ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = "1" ]; then
		arch="arm64"
	fi

	if [ "$version" = "latest" ]; then
		printf 'Resolving latest kute release...\n'
		latest_url="https://github.com/${repo}/releases/latest"
		version="$(curl -fsSLI --proto '=https' --tlsv1.2 -o /dev/null -w '%{url_effective}' "$latest_url" | sed 's#.*/##')"
	fi

	case "$version" in
		v[0-9]*) ;;
		[0-9]*) version="v${version}" ;;
		*) fail "could not resolve release version (got: ${version:-nothing})" ;;
	esac

	archive_version="${version#v}"
	archive="kute_${archive_version}_${os}_${arch}.tar.gz"
	base_url="https://github.com/${repo}/releases/download/${version}"
	tmp="$(mktemp -d 2>/dev/null || mktemp -d -t kute-install)"
	trap cleanup EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	printf 'Installing kute %s for %s/%s...\n' "$version" "$os" "$arch"

	curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "${tmp}/${archive}" "${base_url}/${archive}" || fail "download failed: ${base_url}/${archive}"
	curl -fsSL --proto '=https' --tlsv1.2 --retry 3 -o "${tmp}/checksums.txt" "${base_url}/checksums.txt" || fail "download failed: ${base_url}/checksums.txt"

	verify_signature

	(
		cd "$tmp"
		# Exact filename match on field 2. checksums.txt lists every release
		# artifact, so a substring match for kute_X_linux_amd64.tar.gz would
		# also pull in the .sigstore.json/.sbom.json lines and ask sha256sum
		# to verify files that aren't here. awk compares strings rather than
		# patterns, so the name's dots can't act as wildcards either; the
		# leading '*' is sha256sum's binary-mode marker, which install.ps1
		# tolerates the same way.
		select_sum() { awk -v a="$archive" '$2 == a || $2 == "*" a' checksums.txt; }
		[ -n "$(select_sum)" ] || fail "no checksum entry for ${archive}"
		if command -v sha256sum >/dev/null 2>&1; then
			select_sum | sha256sum -c - >/dev/null || fail "checksum verification failed"
		elif command -v shasum >/dev/null 2>&1; then
			select_sum | shasum -a 256 -c - >/dev/null || fail "checksum verification failed"
		else
			fail "missing required command: sha256sum or shasum"
		fi

		tar -xzf "$archive"
	)

	[ -x "${tmp}/${bin}" ] || fail "archive did not contain executable: ${bin}"

	if [ ! -d "$install_dir" ]; then
		if mkdir -p "$install_dir" 2>/dev/null; then
			:
		elif command -v sudo >/dev/null 2>&1; then
			printf 'Elevating with sudo to create %s\n' "$install_dir"
			sudo mkdir -p "$install_dir"
		else
			fail "could not create install dir: ${install_dir}"
		fi
	fi

	# Stage next to the destination and mv over it: atomic, and replacing
	# the inode avoids ETXTBSY when the old binary is still running.
	if [ -w "$install_dir" ]; then
		cp "${tmp}/${bin}" "${install_dir}/${bin}.new"
		chmod 755 "${install_dir}/${bin}.new"
		mv "${install_dir}/${bin}.new" "${install_dir}/${bin}"
	elif command -v sudo >/dev/null 2>&1; then
		printf 'Elevating with sudo to write %s\n' "$install_dir"
		sudo cp "${tmp}/${bin}" "${install_dir}/${bin}.new"
		sudo chmod 755 "${install_dir}/${bin}.new"
		sudo mv "${install_dir}/${bin}.new" "${install_dir}/${bin}"
	else
		fail "install dir is not writable and sudo is unavailable: ${install_dir}"
	fi

	printf 'kute installed to %s\n' "${install_dir}/${bin}"
	"${install_dir}/${bin}" --version 2>/dev/null || true

	case ":${PATH}:" in
		*":${install_dir}:"*) ;;
		*) printf 'note: %s is not in your PATH\n' "$install_dir" ;;
	esac
}

main "$@"
