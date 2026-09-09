# Installed app

The relay installs as an app from the browser menu on Android, iOS and desktop. The installed app follows the site theme, opens in its own window and keeps the last pages available when the network fails.

## Sharing to the relay

Share a photo, file or link from another app and choose the relay. Shared files wait in the Upload panel on the Files page until you press **Upload**, so nothing is stored without your signature. A shared link opens the **Import from URL** control with the address filled in.

## Large uploads

On browsers that offer it, a file of 8 MiB or more uploads through the browser's background transfer, so the upload continues after the app closes. Every request is signed before the transfer starts, and the browser shows its own progress notice. Open the Files page later to see the result; for an encrypted file the share link appears there. On other browsers the upload runs while the page stays open.

## Nostr links

The app registers for `web+nostr:` links. A link to an `npub`, `nprofile`, `note`, `nevent` or `naddr` opens the matching page: the author's events, the event page or the article. Plain `nostr:` links open the same way through `/open?target=`.

## Notifications

Notifications are off until you turn them on, and each device is enabled separately. Open **Inbox** and choose **Enable on this device**. The browser asks for permission at that moment, and the device is registered with a signed request. Choose **Disable on this device** to stop. The owner can choose **Send a test** to check delivery.

Each device chooses what wakes it: private messages, replies to your posts, issues and pull requests, mentions, and relay notices such as a report awaiting moderation or a finished job. A burst of activity produces one notification per category. The text is encrypted to the device, so the push service never reads it. The app icon shows how many conversation events arrived since you last opened Inbox, and opening Inbox clears it. Devices that stop accepting messages are removed.
