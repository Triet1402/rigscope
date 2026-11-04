import manifest from '../../package.json';

// Bundled with each executable, so this identifies the running code even when
// an installer has since replaced package.json on disk.
export const APPLICATION_VERSION = manifest.version;
