// Admin · user management page copy
const adminUsers = {
  title: 'User Management',
  searchPlaceholder: 'Search email / name…',
  // Fallback message when the API returns no error details
  actionFailed: 'Operation failed',
  // ---- Table headers ----
  colEmail: 'Email',
  colRole: 'Role',
  colProviders: 'Providers',
  colRequests: 'Requests',
  colLastActive: 'Last active',
  // ---- Row actions and messages ----
  promote: 'Promote',
  demote: 'Demote',
  promotedMsg: '{{email}} promoted to admin',
  demotedMsg: '{{email}} demoted to user',
  disabledMsg: '{{email}} disabled (their API keys revoked as well)',
  enabledMsg: '{{email}} enabled',
  resetPassword: 'Reset password',
  noMatch: 'No matching users',
  totalUsers: '{{total}} users in total',
  // ---- Reset password modal ----
  resetPasswordTitle: 'Reset password: {{email}}',
  newPasswordTip: 'New password (shown only once, copy and deliver it now): ',
  done: 'Done',
  resetConfirmTip: 'A 16-character random password will be generated and the current one invalidated immediately. Continue?',
  confirmReset: 'Reset',
}

export default adminUsers
