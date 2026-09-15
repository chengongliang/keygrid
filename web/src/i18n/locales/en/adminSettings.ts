// Admin · system settings page copy
const adminSettings = {
  title: 'System Settings',
  // ---- System announcement ----
  announcement: {
    title: 'System Announcement',
    desc: 'Visible to everyone, shown on the login page and at the top of the app; leave empty to hide.',
    placeholder: 'e.g. System maintenance this Saturday 22:00-23:00, the relay will be unavailable',
    saved: 'Announcement updated',
    save: 'Save announcement',
  },
  // ---- Maintenance mode ----
  maintenance: {
    title: 'Maintenance Mode',
    desc: 'When enabled, the /v1 relay returns 503 plus the announcement for all requests; the admin panel and web app are unaffected.',
    active: 'Under maintenance',
    normal: 'Serving normally',
    openBtn: 'Enter maintenance mode',
    closeBtn: 'Exit maintenance mode',
    confirmMsg: 'Enter maintenance mode? All /v1 relay requests will start returning 503 immediately.',
    openedMsg: 'Maintenance mode enabled',
    exitedMsg: 'Maintenance mode disabled',
  },
  // ---- Registration policy ----
  register: {
    title: 'Registration Policy',
    desc: 'Controls how open /api/auth/register is; the first admin account is not restricted.',
    open: 'Open registration',
    openDesc: 'Anyone can register',
    invite: 'Invite only',
    inviteDesc: 'A valid invitation code is required',
    closed: 'Closed registration',
    closedDesc: 'New sign-ups are disabled',
    saved: 'Registration policy switched to "{{name}}"',
  },
  // ---- Network proxy ----
  proxy: {
    title: 'Network Proxy',
    configured: 'Configured',
    unconfigured: 'Not configured',
    desc: 'Some upstream sites (e.g. OpenAI) require a proxy to reach. Configure a single egress proxy here; users just tick "Access via platform proxy" when adding/editing providers. The address is shown read-only to users (password redacted) and cannot be changed by them. Supports http(s) and socks5; leave empty = no proxy (providers with the proxy option ticked will fail to relay). Takes effect on the relay within 30 seconds after saving.',
    placeholder: 'http://127.0.0.1:7890 or socks5://user:pass@host:1080',
    saved: 'Proxy settings saved',
    save: 'Save proxy',
  },
  // ---- OIDC single sign-on ----
  oidc: {
    title: 'OIDC Single Sign-On',
    off: 'Not enabled',
    desc: 'Works with any standard OIDC IdP (Keycloak / Casdoor / Authentik / Feishu SSO, etc.), Authorization Code + PKCE. Takes effect immediately after saving, no restart needed; platform settings take precedence over OIDC_* environment variables. Employees are provisioned automatically on first IdP login.',
    copyCallback: 'Copy callback URL',
    callbackTip: 'Set the Redirect URI in your IdP client configuration to the callback URL above.',
    enableLabel: 'Enable OIDC login (shows an SSO button on the login page)',
    issuerLabel: 'WellKnown / Issuer URL',
    displayNameLabel: 'Display name (login button text, optional)',
    displayNamePlaceholder: 'Company account',
    hide: 'Hide',
    show: 'Show',
    scopesLabel: 'Scopes (space- or comma-separated; must include openid and email)',
    saved: 'OIDC configuration saved, effective immediately',
    save: 'Save OIDC configuration',
  },
  // ---- Invitation codes ----
  invite: {
    title: 'Invitation Codes',
    desc: 'Used for invite-only registration; valid for 14 days, single use.',
    generate: '+ Generate code',
    colCode: 'Code',
    colExpires: 'Expires at',
    used: 'Used',
    expired: 'Expired',
    available: 'Available',
  },
}

export default adminSettings
