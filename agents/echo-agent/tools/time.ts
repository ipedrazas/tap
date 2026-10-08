// current_time: no arguments; prints JSON on stdout.
process.stdout.write(JSON.stringify({ utc: new Date().toISOString() }));
