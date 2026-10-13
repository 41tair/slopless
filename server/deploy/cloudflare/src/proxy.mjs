function validConnectingIP(value) {
  if (!value || value.length > 45 || /[\s,\r\n]/.test(value)) return false;
  if (value.includes(":")) {
    try {
      return new URL(`http://[${value}]/`).hostname.length > 2;
    } catch {
      return false;
    }
  }
  const octets = value.split(".");
  return octets.length === 4 && octets.every((part) => /^\d{1,3}$/.test(part) && Number(part) <= 255);
}

export function containerRequest(request) {
  const headers = new Headers(request.headers);
  headers.delete("forwarded");
  headers.delete("x-forwarded-for");
  headers.delete("x-real-ip");

  const connectingIP = headers.get("cf-connecting-ip")?.trim();
  if (validConnectingIP(connectingIP)) {
    headers.set("x-forwarded-for", connectingIP);
  }

  return new Request(request, { headers });
}
