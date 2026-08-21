local caller_identity = std.native('caller_identity')();

{
  FunctionName: 'lambroutines-poc',
  Description: 'lambroutines internal-extension PoC',
  Runtime: 'provided.al2023',
  Architectures: ['arm64'],
  Handler: 'bootstrap',
  Role: 'arn:aws:iam::%s:role/AWSLambdaBasicExecutionRole' % [caller_identity.Account],
  MemorySize: 128,
  Timeout: 15,
}
