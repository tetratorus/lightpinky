import Foundation

/// Bounded recovery for observations. Cancellation of an Apple async call is
/// cooperative: abandoned attempts keep their slot until they actually finish.
/// Share this object across reconnects so a hung framework cannot accumulate work.
final class ReadRecovery<Value> {
  private let lock = NSLock()
  private var active = 0
  private let operation: String

  init(_ operation: String) { self.operation = operation }

  private final class Reply {
    private let lock = NSLock()
    private let ready = DispatchSemaphore(value: 0)
    private var result: Result<Value, Error>?
    private var abandoned = false

    func finish(_ value: Result<Value, Error>) {
      lock.lock()
      if !abandoned { result = value }
      lock.unlock()
      ready.signal()
    }

    func wait(_ seconds: Double) -> Result<Value, Error>? {
      guard ready.wait(timeout: .now() + seconds) == .success else { return nil }
      lock.lock(); defer { lock.unlock() }
      return result
    }

    func abandon() {
      lock.lock(); abandoned = true; result = nil; lock.unlock()
    }
  }

  private func start(_ read: @escaping () async throws -> Value) throws -> (Reply, Task<Void, Never>) {
    lock.lock()
    guard active < 2 else {
      lock.unlock()
      throw CoreConnectionFailure.timedOut("\(operation): previous canceled reads are still pending")
    }
    active += 1
    lock.unlock()
    let reply = Reply()
    let task = Task {
      let result: Result<Value, Error>
      do {
        try Task.checkCancellation()
        let value = try await read()
        try Task.checkCancellation()
        result = .success(value)
      } catch { result = .failure(error) }
      release()
      reply.finish(result)
    }
    return (reply, task)
  }

  private func release() {
    lock.lock(); active -= 1; lock.unlock()
  }

  /// A fast response returns immediately. After one stalled attempt, cancel
  /// and replace it once. Late results belong only to their abandoned reply.
  func read(firstTimeout: Double = 1, replacementTimeout: Double = 3,
            _ read: @escaping () async throws -> Value) throws -> Value {
    let began = ProcessInfo.processInfo.systemUptime
    for (index, timeout) in [firstTimeout, replacementTimeout].enumerated() {
      let (reply, task) = try start(read)
      if let result = reply.wait(timeout) {
        do { return try result.get() }
        catch is CoreConnectionFailure where index == 0 { continue }
      }
      reply.abandon()
      task.cancel()
      if index == 0 { NativeTiming.record("\(operation): canceled stalled read, retrying", since: began) }
    }
    throw CoreConnectionFailure.timedOut("\(operation) after replacement read")
  }
}
