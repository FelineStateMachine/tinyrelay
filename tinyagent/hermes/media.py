"""Validated Tinyrelay room media handling for the Hermes adapter."""

from __future__ import annotations

import hashlib
import re
from typing import Any
from urllib.parse import urlsplit

MAX_MEDIA_BYTES = 32 << 20
_MEDIA_PATH = re.compile(r"^/media/([0-9a-f]{64})(?:\.[a-z0-9]{1,8})?$", re.I)


def imeta_fields(tag: list[str]) -> dict[str, str]:
    """Parse NIP-92 ``key value`` fields from an imeta tag."""
    fields: dict[str, str] = {}
    for item in tag[1:]:
        key, separator, value = str(item).partition(" ")
        if separator and key and value:
            fields.setdefault(key.lower(), value)
    return fields


def _media_path(raw_url: str, relay_url: str) -> tuple[str, str] | None:
    target = urlsplit(raw_url)
    origin = urlsplit(relay_url)
    if target.query or target.fragment or origin.query or origin.fragment:
        return None
    if target.scheme or target.netloc:
        if (target.scheme, target.netloc) != (origin.scheme, origin.netloc):
            return None
        path = target.path
    elif raw_url.startswith("/"):
        path = target.path
    else:
        return None
    prefix = origin.path.rstrip("/")
    if prefix:
        if not path.startswith(prefix + "/"):
            return None
        path = path[len(prefix):]
    match = _MEDIA_PATH.fullmatch(path)
    return (path, match.group(1).lower()) if match else None


async def incoming_media(rpc: Any, event: dict, relay_url: str) -> tuple[list[str], list[str]]:
    """Download and cache validated same-relay imeta attachments.

    Tiny authorization is enforced by the signed helper download. The URL and
    digest checks ensure a message cannot make Hermes fetch arbitrary internet
    content or cache bytes under a false identity.
    """
    from gateway.platforms.base import cache_media_bytes_async

    urls: list[str] = []
    types: list[str] = []
    for tag in event.get("tags", []):
        if not isinstance(tag, list) or len(tag) < 2 or tag[0] != "imeta":
            continue
        fields = imeta_fields(tag)
        location = _media_path(fields.get("url", ""), relay_url)
        if location is None:
            continue
        path, digest = location
        declared = fields.get("x", "").lower()
        if declared and declared != digest:
            continue
        try:
            data, mime = await rpc.download(path)
        except Exception:
            continue
        if len(data) > MAX_MEDIA_BYTES or hashlib.sha256(data).hexdigest() != digest:
            continue
        try:
            if fields.get("size") and int(fields["size"]) != len(data):
                continue
        except ValueError:
            continue
        cached = await cache_media_bytes_async(
            data,
            filename=fields.get("filename", "attachment"),
            mime_type=fields.get("m", mime),
        )
        if cached:
            urls.append(cached.path)
            types.append(cached.media_type)
    return urls, types
