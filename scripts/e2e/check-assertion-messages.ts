/** Offline privacy gate for E2E assertion messages, called by check_all.sh.
 * Uses the pinned TS parser; reports only locations, never source or body values.
 * Only literal prose or locally proven primitive labels are allowed as messages.
 */
import ts from 'typescript-parser';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

export function violations(source: string): number[] {
  const file = ts.createSourceFile('spec.ts', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  const definitions = new Map<string, ts.Expression>();
  const expects = new Set(['expect']);
  const walk = (node: ts.Node, visit: (node: ts.Node) => void) => { visit(node); ts.forEachChild(node, child => walk(child, visit)); };
  walk(file, node => {
    if (ts.isImportSpecifier(node) && (node.propertyName ?? node.name).text === 'expect') expects.add(node.name.text);
    if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.initializer &&
        ts.isVariableDeclarationList(node.parent) && (node.parent.flags & ts.NodeFlags.Const))
      definitions.set(node.name.text, node.initializer);
  });
  for (let i = 0; i < definitions.size; i++)
    for (const [name, value] of definitions)
      if (expects.has(value.getText(file))) expects.add(name);
  function safe(node: ts.Node, seen = new Set<string>()): boolean {
    if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node) || ts.isNumericLiteral(node)) return true;
    if (ts.isIdentifier(node)) {
      if (/body|response|payload/i.test(node.text) || seen.has(node.text)) return false;
      const value = definitions.get(node.text);
      return !!value && safe(value, new Set([...seen, node.text]));
    }
    if (ts.isTemplateExpression(node)) return node.templateSpans.every(span => safe(span.expression, seen));
    if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.PlusToken)
      return safe(node.left, seen) && safe(node.right, seen);
    return false; // Calls, mutable aliases and unknown expressions fail closed.
  }
  const lines: number[] = [];
  walk(file, node => {
    if (ts.isCallExpression(node) &&
        [...expects].some(name => [name, name + '.soft', name + '.poll'].includes(node.expression.getText(file))) &&
        node.arguments[1]) {
      const arg = node.arguments[1];
      const message = ts.isObjectLiteralExpression(arg)
        ? arg.properties.find(prop => ts.isSpreadAssignment(prop) ||
          (prop.name && (ts.isComputedPropertyName(prop.name) || prop.name.getText(file).replace(/["']/g, '') === 'message')))
        : arg;
      if (message && !safe(ts.isPropertyAssignment(message) ? message.initializer : message))
        lines.push(file.getLineAndCharacterOfPosition(arg.getStart(file)).line + 1);
    }
  });
  return lines;
}
const paths = process.argv.slice(2);
for (const path of paths.length ? paths : readdirSync('tests/e2e').filter(name => name.endsWith('.ts')).map(name => join('tests/e2e', name))) {
  for (const line of violations(readFileSync(path, 'utf8'))) {
    console.error(`FAIL: unsafe assertion message ${path}:${line}`); process.exitCode = 1;
  }
}
