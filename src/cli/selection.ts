import { existsSync } from 'node:fs';

export type InstallationScope = 'user' | 'system';

/** Discovery never opens private system credentials or initializes state. */
export function selectInstallation(options: {
  scope?: string;
  explicitState: boolean;
  userPresent: boolean;
  systemPresent: boolean;
}): InstallationScope {
  if (options.scope !== undefined && options.scope !== 'user' && options.scope !== 'system') {
    throw new Error('--scope must be user or system.');
  }
  if (options.scope === 'system' && options.explicitState) {
    throw new Error('--state-dir selects a user installation. Use rigscope system for custom system paths.');
  }
  if (options.scope) return options.scope as InstallationScope;
  if (options.explicitState) return 'user';
  if (options.userPresent && options.systemPresent) {
    throw new Error('Both user and system installations exist. Choose --scope user or --scope system.');
  }
  return options.systemPresent ? 'system' : 'user';
}

export function systemInstallationPresent(): boolean {
  return existsSync('/opt/rigscope/current') ||
    existsSync('/etc/systemd/system/rigscope-hub.service') ||
    existsSync('/etc/systemd/system/rigscope-collector.service');
}
