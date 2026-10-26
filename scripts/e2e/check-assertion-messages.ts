/** Offline privacy gate for E2E assertion messages, called by check_all.sh.
 * Uses the pinned TS parser; reports only locations, never source or body values.
 * Only inline literal expressions are allowed as messages; identifiers fail closed.
 */
import ts from 'typescript-parser';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';

export function violations(source: string): number[] {
  const file = ts.createSourceFile('spec.ts', source, ts.ScriptTarget.Latest, true, ts.ScriptKind.TS);
  const definitions = new Map<string, ts.Expression | null>();
  const expects = new Set(['expect']);
  const walk = (node: ts.Node, visit: (node: ts.Node) => void) => { visit(node); ts.forEachChild(node, child => walk(child, visit)); };
  walk(file, node => {
    if (ts.isImportSpecifier(node) && (node.propertyName ?? node.name).text === 'expect') expects.add(node.name.text);
    if (ts.isVariableDeclaration(node) && ts.isIdentifier(node.name) && node.initializer &&
        ts.isVariableDeclarationList(node.parent) && (node.parent.flags & ts.NodeFlags.Const))
      definitions.set(node.name.text, definitions.has(node.name.text) ? null : node.initializer);
  });
  for (let i = 0; i < definitions.size; i++)
    for (const [name, value] of definitions)
      if (value && expects.has(value.getText(file))) expects.add(name);
  function safe(node: ts.Node): boolean {
    if (ts.isStringLiteral(node) || ts.isNoSubstitutionTemplateLiteral(node) || ts.isNumericLiteral(node)) return true;
    if (ts.isTemplateExpression(node)) return node.templateSpans.every(span => safe(span.expression));
    if (ts.isBinaryExpression(node) && node.operatorToken.kind === ts.SyntaxKind.PlusToken)
      return safe(node.left) && safe(node.right);
    return false; // Identifiers (including shadowed constants), calls and unknown expressions fail closed.
  }
  const lines: number[] = [];
  walk(file, node => {
    if (ts.isCallExpression(node) &&
        [...expects].some(name => [name, name + '.soft', name + '.poll'].includes(node.expression.getText(file))) &&
        node.arguments[1]) {
      const arg = node.arguments[1];
      const messages = ts.isObjectLiteralExpression(arg)
        ? arg.properties.filter(prop => ts.isSpreadAssignment(prop) ||
          (prop.name && (ts.isComputedPropertyName(prop.name) || prop.name.getText(file).replace(/["']/g, '') === 'message')))
        : [arg];
      if (messages.some(message => !safe(ts.isPropertyAssignment(message) ? message.initializer : message)))
        lines.push(file.getLineAndCharacterOfPosition(arg.getStart(file)).line + 1);
    }
  });
  return lines;
}
const paths = process.argv.slice(2);
for (const path of paths.length ? paths : readdirSync('tests/e2e', { recursive: true, encoding: 'utf8' }).filter(name => name.endsWith('.ts')).map(name => join('tests/e2e', name))) {
  for (const line of violations(readFileSync(path, 'utf8'))) {
    console.error(`FAIL: unsafe assertion message ${path}:${line}`); process.exitCode = 1;
  }
}
