// Review probe for PR #103: proves code runs before/inside the golden writer via an un-allowlisted input.
console.error('PROBE103-PRELOAD pid=' + process.pid + ' argv=' + JSON.stringify(process.argv.slice(1)));
