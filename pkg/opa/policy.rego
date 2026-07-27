##############################################################################
# Package: envoy.authz
#
# This OPA policy is evaluated by the Envoy ext_authz gRPC filter.
# Envoy sends a CheckRequest and expects a CheckResponse indicating
# whether the request is allowed or denied.
#
# Input shape (envoy.service.auth.v3.CheckRequest):
#   input.attributes.request.http.method     — HTTP method
#   input.attributes.request.http.path       — URL path
#   input.attributes.request.http.headers    — map of lowercased header names
#   input.attributes.source.principal        — mTLS SPIFFE identity (if present)
##############################################################################
package envoy.authz

import future.keywords.if
import future.keywords.in

# ---------------------------------------------------------------------------
# Default: deny everything
# ---------------------------------------------------------------------------
default allow := false

# ---------------------------------------------------------------------------
# Rule 1: Allow if JWT bearer token carries an authorised role
# ---------------------------------------------------------------------------
allow if {
    # Decode the JWT (no signature verification — use OPA's built-in JWKS
    # verification in production by replacing with io.jwt.decode_verify)
    [_, payload, _] := io.jwt.decode(bearer_token)

    # The token must not be expired
    not token_expired(payload)

    # The caller must hold at least one approved role
    payload.roles[_] in allowed_roles
}

# ---------------------------------------------------------------------------
# Rule 2: Allow health-check paths unconditionally (no auth required)
# ---------------------------------------------------------------------------
allow if {
    startswith(input.attributes.request.http.path, "/healthz")
}

allow if {
    startswith(input.attributes.request.http.path, "/readyz")
}

# ---------------------------------------------------------------------------
# Rule 3: Allow mTLS service-to-service calls from known SPIFFE identities
# ---------------------------------------------------------------------------
allow if {
    principal := input.attributes.source.principal
    startswith(principal, "spiffe://cluster.local/ns/")
    # Restrict to services in the platform or commerce namespaces
    allowed_spiffe_prefixes[pfx]
    startswith(principal, pfx)
}

# ---------------------------------------------------------------------------
# Role allow-list
# ---------------------------------------------------------------------------
allowed_roles := {
    "platform-admin",
    "service-account",
    "developer",
    "readonly",
}

# ---------------------------------------------------------------------------
# SPIFFE identity prefixes that are trusted for mTLS passthrough
# ---------------------------------------------------------------------------
allowed_spiffe_prefixes := {
    "spiffe://cluster.local/ns/platform/sa/",
    "spiffe://cluster.local/ns/default/sa/",
    "spiffe://cluster.local/ns/commerce/sa/",
}

# ---------------------------------------------------------------------------
# Helper: extract Bearer token from Authorization header
# ---------------------------------------------------------------------------
bearer_token := token if {
    auth_header := input.attributes.request.http.headers.authorization
    startswith(auth_header, "Bearer ")
    token := substring(auth_header, 7, -1)
}

# ---------------------------------------------------------------------------
# Helper: check token expiry (exp claim is Unix seconds)
# ---------------------------------------------------------------------------
token_expired(payload) if {
    now_sec := time.now_ns() / 1000000000
    payload.exp < now_sec
}

# ---------------------------------------------------------------------------
# Helper: build a structured denial reason returned in the gRPC response
# ---------------------------------------------------------------------------
denial_reason := reason if {
    not allow
    reason := "Unauthorized: valid Bearer token with approved role required"
}

denial_reason := "" if {
    allow
}
