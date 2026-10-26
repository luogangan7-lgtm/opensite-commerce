/** Public CI output boundary: source metadata and finite outcomes only.
 * Depends on Playwright's reporter API and the pinned TS parser; used by MVP CI.
 * Never serializes errors, runtime titles, IO, step titles/params or attachments.
 */
import type { Reporter, TestCase, TestResult, TestStep } from '@playwright/test/reporter';
import ts from 'typescript-parser';
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, relative, resolve } from 'node:path';

export default class SummaryReporter implements Reporter {
  private rows: object[] = [];
  private sources = new Map<string, ts.SourceFile>();
  constructor(private options: { outputFile: string }) {}
  printsToStdio() { return true; }
  // Explicitly consume runtime IO/global errors without forwarding any payload.
  onStdOut() {}
  onStdErr() {}
  onError() {}

  private source(location?: TestCase['location']) {
    if (!location || !Number.isSafeInteger(location.line) || location.line < 1) return null;
    const file = relative(process.cwd(), resolve(location.file)).replaceAll('\\', '/');
    if (!/^tests\/e2e\/[a-z0-9/-]+(?:\.[a-z]+)*\.ts$/.test(file)) return null;
    try {
      let ast = this.sources.get(file);
      if (!ast) {
        ast = ts.createSourceFile(file, readFileSync(file, 'utf8'), ts.ScriptTarget.Latest, true);
        this.sources.set(file, ast);
      }
      if (location.line > ast.getLineStarts().length) return null;
      return { file, ast, line: location.line };
    } catch { return null; }
  }

  onTestEnd(test: TestCase, result: TestResult) {
    const source = this.source(test.location);
    const titlePath: string[] = [];
    if (source) {
      const visit = (node: ts.Node) => {
        if (ts.isCallExpression(node) && /^test(?:\.describe|\.skip|\.fixme)?$/.test(node.expression.getText(source.ast))) {
          const start = source.ast.getLineAndCharacterOfPosition(node.getStart(source.ast)).line + 1;
          const end = source.ast.getLineAndCharacterOfPosition(node.end).line + 1;
          const title = node.arguments[0];
          if (start <= source.line && source.line <= end && title) {
            // Templates are source templates, never evaluated runtime substitutions.
            if (ts.isStringLiteral(title) || ts.isNoSubstitutionTemplateLiteral(title)) titlePath.push(title.text);
            else if (ts.isTemplateExpression(title)) titlePath.push(title.getText(source.ast));
          }
        }
        ts.forEachChild(node, visit);
      };
      visit(source.ast);
    }
    const failedStep = (steps: TestStep[]): TestStep | undefined => {
      for (const step of steps) {
        const child = failedStep(step.steps);
        if (child) return child;
        if (step.error && step.category === 'expect') return step;
      }
    };
    const failed = failedStep(result.steps);
    const error = this.source(failed?.location ?? result.error?.location);
    let label: string | null = null;
    if (error) {
      const visit = (node: ts.Node) => {
        if (ts.isCallExpression(node) && /^expect(?:\.soft|\.poll)?$/.test(node.expression.getText(error.ast))) {
          const start = error.ast.getLineAndCharacterOfPosition(node.getStart(error.ast)).line + 1;
          const end = error.ast.getLineAndCharacterOfPosition(node.end).line + 1;
          if (start <= error.line && error.line <= end) {
            let message: ts.Expression | undefined = node.arguments[1];
            if (message && ts.isObjectLiteralExpression(message)) {
              const property = message.properties.find(p => ts.isPropertyAssignment(p) && p.name.getText(error.ast).replace(/["']/g, '') === 'message');
              message = property && ts.isPropertyAssignment(property) ? property.initializer : undefined;
            }
            if (message && (ts.isStringLiteral(message) || ts.isNoSubstitutionTemplateLiteral(message))) label = message.text;
          }
        }
        ts.forEachChild(node, visit);
      };
      visit(error.ast);
    }
    const project = test.parent.project()?.name;
    const row = {
      file: source?.file ?? null, line: source?.line ?? null, title_path: titlePath,
      project: project === 'desktop' || project === 'mobile' ? project : null,
      status: ['passed', 'failed', 'timedOut', 'skipped', 'interrupted'].includes(result.status) ? result.status : null,
      duration_ms: Number.isFinite(result.duration) && result.duration >= 0 ? Math.round(result.duration) : null,
      error_location: error ? `${error.file}:${error.line}` : null, assertion_label: label,
    };
    this.rows.push(row);
    this.onEnd();
    console.log(JSON.stringify(row));
  }
  onEnd() {
    mkdirSync(dirname(this.options.outputFile), { recursive: true });
    writeFileSync(this.options.outputFile, JSON.stringify(this.rows) + '\n');
  }
}
