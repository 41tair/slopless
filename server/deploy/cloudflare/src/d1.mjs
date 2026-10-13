const SQLITE = Object.freeze({
  ERROR: 1,
  BUSY: 5,
  LOCKED: 6,
  READONLY: 8,
  INTERRUPT: 9,
  IOERR: 10,
  CORRUPT: 11,
  FULL: 13,
  CANTOPEN: 14,
  SCHEMA: 17,
  TOOBIG: 18,
  CONSTRAINT: 19,
  MISMATCH: 20,
  MISUSE: 21,
  RANGE: 25,
  NOTADB: 26
});

const MAX_STATEMENTS = 128;
const BLOB_PATTERN = /^[0-9A-F]*$/;
const READ_ONLY_PRAGMAS = new Set([
  "collation_list",
  "compile_options",
  "database_list",
  "foreign_key_list",
  "function_list",
  "index_info",
  "index_list",
  "index_xinfo",
  "module_list",
  "pragma_list",
  "table_info",
  "table_list",
  "table_xinfo"
]);

class RequestError extends Error {
  constructor(message) {
    super(message);
    this.name = "RequestError";
  }
}

function jsonResponse(body, status = 200) {
  return Response.json(body, {
    status,
    headers: {
      "cache-control": "no-store"
    }
  });
}

function errorEnvelope(code, message) {
  return {
    result: [],
    success: false,
    errors: [{ code, message }],
    messages: []
  };
}

function decodeHex(hex) {
  if (!BLOB_PATTERN.test(hex) || hex.length % 2 !== 0) {
    throw new RequestError("invalid blob parameter");
  }

  const value = new Uint8Array(hex.length / 2);
  for (let index = 0; index < hex.length; index += 2) {
    value[index / 2] = Number.parseInt(hex.slice(index, index + 2), 16);
  }
  return value;
}

export function decodeParameter(value) {
  if (value === null || typeof value === "string" || typeof value === "boolean") {
    return value;
  }

  if (typeof value === "number") {
    if (!Number.isFinite(value) || !Number.isSafeInteger(value) && Number.isInteger(value)) {
      throw new RequestError("invalid numeric parameter");
    }
    return value;
  }

  if (
    value &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    Object.keys(value).length === 1 &&
    typeof value.blob === "string"
  ) {
    return decodeHex(value.blob);
  }

  throw new RequestError("unsupported parameter type");
}

function parseStatement(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new RequestError("invalid statement");
  }
  if (typeof value.sql !== "string" || value.sql.trim() === "") {
    throw new RequestError("invalid statement SQL");
  }
  if (value.params !== undefined && !Array.isArray(value.params)) {
    throw new RequestError("invalid statement parameters");
  }

  return {
    sql: value.sql,
    params: (value.params ?? []).map(decodeParameter)
  };
}

function parseBatch(body) {
  if (!body || typeof body !== "object" || !Array.isArray(body.batch)) {
    throw new RequestError("batch is required");
  }
  if (body.batch.length === 0 || body.batch.length > MAX_STATEMENTS) {
    throw new RequestError("invalid batch size");
  }
  return body.batch.map(parseStatement);
}

function stripLeadingSpaceAndComments(sql) {
  let rest = sql;
  for (;;) {
    rest = rest.trimStart();
    if (rest.startsWith("--")) {
      const newline = rest.indexOf("\n", 2);
      return newline === -1 ? "" : stripLeadingSpaceAndComments(rest.slice(newline + 1));
    }
    if (rest.startsWith("/*")) {
      const end = rest.indexOf("*/", 2);
      if (end === -1) {
        throw new RequestError("unterminated SQL comment");
      }
      rest = rest.slice(end + 2);
      continue;
    }
    return rest;
  }
}

function hasMoreAfterTerminator(sql, offset) {
  return stripLeadingSpaceAndComments(sql.slice(offset)).length !== 0;
}

export function assertSingleReadOnlyStatement(sql) {
  const leading = stripLeadingSpaceAndComments(sql);
  const firstWord = /^([A-Za-z]+)/.exec(leading)?.[1]?.toUpperCase();
  if (firstWord === "PRAGMA") {
    const pragma = /^PRAGMA\s+(?:[A-Za-z_][\w]*\.)?([A-Za-z_][\w]*)/i.exec(leading)?.[1]?.toLowerCase();
    if (!pragma || !READ_ONLY_PRAGMAS.has(pragma) || /=/.test(leading)) {
      throw new RequestError("raw accepts read-only SQL");
    }
  } else if (firstWord !== "SELECT" && firstWord !== "EXPLAIN") {
    throw new RequestError("raw accepts read-only SQL");
  }

  let quote = "";
  for (let index = 0; index < sql.length; index += 1) {
    const current = sql[index];
    const next = sql[index + 1];

    if (quote) {
      if (quote === "]" && current === "]") {
        quote = "";
      } else if (quote !== "]" && current === quote) {
        if (next === quote) {
          index += 1;
        } else {
          quote = "";
        }
      }
      continue;
    }

    if (current === "'" || current === "\"" || current === "`") {
      quote = current;
      continue;
    }
    if (current === "[") {
      quote = "]";
      continue;
    }
    if (current === "-" && next === "-") {
      const newline = sql.indexOf("\n", index + 2);
      if (newline === -1) return;
      index = newline;
      continue;
    }
    if (current === "/" && next === "*") {
      const end = sql.indexOf("*/", index + 2);
      if (end === -1) throw new RequestError("unterminated SQL comment");
      index = end + 1;
      continue;
    }
    if (current === ";" && hasMoreAfterTerminator(sql, index + 1)) {
      throw new RequestError("raw accepts one statement");
    }
  }

  if (quote) throw new RequestError("unterminated SQL quote");
}

export function jsonSerializable(value) {
  if (value instanceof ArrayBuffer) {
    return Array.from(new Uint8Array(value));
  }
  if (ArrayBuffer.isView(value)) {
    return Array.from(new Uint8Array(value.buffer, value.byteOffset, value.byteLength));
  }
  if (Array.isArray(value)) {
    return value.map(jsonSerializable);
  }
  if (value && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, jsonSerializable(item)]));
  }
  return value;
}

function sqliteCode(error) {
  const text = `${error?.name ?? ""} ${error?.message ?? ""}`.toUpperCase();
  const patterns = [
    ["SQLITE_CONSTRAINT", SQLITE.CONSTRAINT],
    ["SQLITE_BUSY", SQLITE.BUSY],
    ["SQLITE_LOCKED", SQLITE.LOCKED],
    ["SQLITE_READONLY", SQLITE.READONLY],
    ["SQLITE_INTERRUPT", SQLITE.INTERRUPT],
    ["SQLITE_IOERR", SQLITE.IOERR],
    ["SQLITE_CORRUPT", SQLITE.CORRUPT],
    ["SQLITE_FULL", SQLITE.FULL],
    ["SQLITE_CANTOPEN", SQLITE.CANTOPEN],
    ["SQLITE_SCHEMA", SQLITE.SCHEMA],
    ["SQLITE_TOOBIG", SQLITE.TOOBIG],
    ["SQLITE_MISMATCH", SQLITE.MISMATCH],
    ["SQLITE_MISUSE", SQLITE.MISUSE],
    ["SQLITE_RANGE", SQLITE.RANGE],
    ["SQLITE_NOTADB", SQLITE.NOTADB]
  ];
  if (text.includes("SQLITE_ERROR")) return SQLITE.ERROR;
  return patterns.find(([pattern]) => text.includes(pattern))?.[1];
}

function errorStatus(code) {
  if (code === SQLITE.CONSTRAINT) return 409;
  if ([SQLITE.BUSY, SQLITE.LOCKED, SQLITE.INTERRUPT, SQLITE.IOERR, SQLITE.CANTOPEN].includes(code)) {
    return 503;
  }
  // Unknown failures include binding/network failures whose Error text has no
  // SQLite result code. Keep them retryable without exposing their details.
  return code === undefined ? 503 : 400;
}

async function requestBody(request) {
  if (!request.headers.get("content-type")?.toLowerCase().startsWith("application/json")) {
    throw new RequestError("application/json is required");
  }
  try {
    return await request.json();
  } catch {
    throw new RequestError("invalid JSON body");
  }
}

function prepare(db, statement) {
  return db.prepare(statement.sql).bind(...statement.params);
}

async function executeQuery(request, db) {
  const statements = parseBatch(await requestBody(request));
  const result = await db.batch(statements.map((statement) => prepare(db, statement)));
  return jsonResponse({
    result: jsonSerializable(result),
    success: true,
    errors: [],
    messages: []
  });
}

async function executeRaw(request, db) {
  const body = await requestBody(request);
  const statement = parseStatement(body);
  assertSingleReadOnlyStatement(statement.sql);
  const raw = await prepare(db, statement).raw({ columnNames: true });
  const [columns = [], ...rows] = jsonSerializable(raw);
  return jsonResponse({
    result: [{
      success: true,
      results: { columns, rows },
      meta: { changes: 0, last_row_id: 0 }
    }],
    success: true,
    errors: [],
    messages: []
  });
}

export async function handleD1Request(request, db) {
  const url = new URL(request.url);
  if (request.method !== "POST") {
    return jsonResponse(errorEnvelope(SQLITE.MISUSE, "method not allowed"), 405);
  }

  try {
    if (url.pathname === "/query") return await executeQuery(request, db);
    if (url.pathname === "/raw") return await executeRaw(request, db);
    return jsonResponse(errorEnvelope(SQLITE.MISUSE, "route not found"), 404);
  } catch (error) {
    if (error instanceof RequestError) {
      return jsonResponse(errorEnvelope(SQLITE.MISUSE, error.message), 400);
    }
    const detectedCode = sqliteCode(error);
    const code = detectedCode ?? SQLITE.ERROR;
    const status = errorStatus(detectedCode);
    const message = code === SQLITE.CONSTRAINT ? "D1 constraint failed" : "D1 binding request failed";
    return jsonResponse(errorEnvelope(code, message), status);
  }
}
