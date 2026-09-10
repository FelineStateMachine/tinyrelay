// Package webpush implements browser push delivery for the relay.
//
// Subscription validates an HTTPS endpoint, an uncompressed P-256 subscriber
// key and its 16-byte authentication secret. Keys stores the relay's P-256
// VAPID key and serializes it as unpadded base64url PKCS#8. Encrypt implements
// RFC 8291 aes128gcm payload encryption. Send builds the RFC 8292 VAPID
// authorization, posts the encrypted record, and reports whether the service
// accepted or retired the subscription.
package webpush
