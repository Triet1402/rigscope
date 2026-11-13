// Package config owns Rigscope's versioned configuration file: its schema,
// defaults, validation, atomic persistence, path resolution for the user and
// system scopes, and forward migrations.
//
// It does not own installation decisions (enablement presets, units, service
// management) or the runtime settings of the hub and collector beyond the
// values stored here.
package config
