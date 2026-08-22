const GATEWAY_URL = process.env.CONDUIT_URL ?? "http://localhost:8080";

type RouteContext = { params: Promise<{ path: string[] }> };

async function forward(request: Request, context: RouteContext) {
  const { path } = await context.params;
  const incoming = new URL(request.url);
  const target = `${GATEWAY_URL}/control/v1/${path.join("/")}${incoming.search}`;
  const headers = new Headers({ Accept: "application/json" });
  const cookie = request.headers.get("cookie");
  const csrf = request.headers.get("x-csrf-token");
  const contentType = request.headers.get("content-type");
  if (cookie) headers.set("cookie", cookie);
  if (csrf) headers.set("x-csrf-token", csrf);
  if (contentType) headers.set("content-type", contentType);

  try {
    const response = await fetch(target, {
      method: request.method,
      headers,
      body: request.method === "GET" || request.method === "HEAD" ? undefined : await request.arrayBuffer(),
      cache: "no-store",
      signal: AbortSignal.timeout(15_000),
    });
    const outgoing = new Headers();
    outgoing.set("content-type", response.headers.get("content-type") ?? "application/json");
    const setCookie = response.headers.get("set-cookie");
    if (setCookie) outgoing.set("set-cookie", setCookie);
    return new Response(response.body, { status: response.status, headers: outgoing });
  } catch (error) {
    const message = error instanceof Error ? error.message : "gateway unavailable";
    return Response.json({ error: { code: "gateway_unavailable", message } }, { status: 503 });
  }
}

export const dynamic = "force-dynamic";
export const GET = forward;
export const POST = forward;
export const PUT = forward;
export const PATCH = forward;
export const DELETE = forward;
