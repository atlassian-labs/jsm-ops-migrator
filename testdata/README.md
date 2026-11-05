# testdata

Fixtures and golden files. Everything here is scrubbed: names and emails are
deterministic pseudonyms, phone numbers are masked, and no token, routing key,
device detail or calendar URL appears. CI scans this folder and fails on any
secret or unmasked phone number.
