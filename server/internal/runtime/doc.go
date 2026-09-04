// Package runtime serves the runtime list: which machines have paired with an
// organization and which of them are reachable right now.
//
// `online` is derived from last_seen_at at read time, never stored. A daemon
// that is SIGKILLed, suspended with the laptop lid, or cut off by a firewall
// change never gets to write a status column on its way out, so a stored flag
// would say "online" forever.
package runtime
