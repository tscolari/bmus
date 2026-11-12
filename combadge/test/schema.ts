import * as fs from "node:fs";
import { fileURLToPath } from "node:url";
import { Ajv2020 } from "ajv/dist/2020.js";
import addFormatsModule from "ajv-formats";

export const protocolDir = fileURLToPath(new URL("../../protocol/", import.meta.url));

// ajv-formats is CommonJS; under nodenext its default import is the module object.
const addFormats = addFormatsModule as unknown as typeof addFormatsModule.default;

/** Compiled protocol/messages.schema.json. Tests only; serve never validates at runtime. */
export function compileSchema() {
  const ajv = new Ajv2020({ strict: true, allErrors: true });
  addFormats(ajv);
  const schema = JSON.parse(fs.readFileSync(`${protocolDir}messages.schema.json`, "utf8")) as object;
  return { schema, validate: ajv.compile(schema) };
}
