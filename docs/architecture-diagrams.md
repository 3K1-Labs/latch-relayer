# Latch Relayer Architecture Diagrams

GitHub renders Mermaid diagrams natively in Markdown files. Each diagram below is intentionally focused on one question so the architecture stays easy to explain and maintain.

## 1. System Context

This shows where `latch-relayer` sits in the broader Latch and Stellar system.

```mermaid
flowchart LR
    User["User"]
    OnRamp["On-ramp / Exchange / Wallet"]
    LatchAPI["latch-api"]
    Relayer["latch-relayer<br/>Go service on Render"]
    DB[("PostgreSQL / Neon")]
    Horizon["Stellar Horizon<br/>payment stream"]
    RPC["Stellar RPC<br/>Soroban simulation"]
    Pool["Pool G-address<br/>signer + fee payer"]
    SAC["Native XLM SAC<br/>Soroban transfer"]
    CAddress["User C-address<br/>Soroban contract address"]
    Recovery["Recovery G-address"]

    User -->|"Starts funding session"| LatchAPI
    LatchAPI -->|"POST /intents"| Relayer
    Relayer -->|"Create intent + memo_id"| DB
    Relayer -->|"Return pool address + memo_id"| LatchAPI
    LatchAPI -->|"Show deposit details"| User

    User -->|"G-address + memo_id"| OnRamp
    OnRamp -->|"Stellar payment"| Pool
    Horizon -->|"SSE payment events"| Relayer
    Relayer -->|"Read/write intents, forwards, cursors"| DB
    Relayer -->|"Simulate Soroban tx"| RPC
    Relayer -->|"Submit signed SAC transfer"| Horizon
    Pool -->|"Funds source"| SAC
    SAC -->|"Transfer XLM"| CAddress

    Relayer -->|"Invalid / unknown / expired deposits"| Recovery
```

## 2. Happy Path Sequence

This is the normal successful flow from intent creation through deposit forwarding.

```mermaid
sequenceDiagram
    autonumber
    actor User
    participant API as latch-api
    participant Relayer as latch-relayer
    participant DB as PostgreSQL
    participant Sender as On-ramp / Wallet
    participant Horizon as Stellar Horizon
    participant RPC as Stellar RPC
    participant SAC as Native XLM SAC
    participant CAddr as User C-address

    User->>API: Start funding session
    API->>Relayer: POST /intents
    Relayer->>DB: Insert pending intent with random memo_id
    DB-->>Relayer: intent_id, memo_id, pool_address, expires_at
    Relayer-->>API: Intent response
    API-->>User: Show pool G-address + memo_id

    User->>Sender: Send XLM using pool address + memo_id
    Sender->>Horizon: Submit Stellar payment
    Horizon-->>Relayer: SSE payment event with memo
    Relayer->>DB: Insert forward record by tx_hash
    Relayer->>DB: Lookup pending intent by memo_id
    Relayer->>RPC: Simulate SAC transfer
    RPC-->>Relayer: Footprint + resource fee
    Relayer->>Horizon: Submit signed Soroban SAC transfer
    Horizon->>SAC: Invoke transfer
    SAC->>CAddr: Credit XLM
    Horizon-->>Relayer: Forward tx hash
    Relayer->>DB: Mark forward done and intent completed

    API->>Relayer: GET /deposit/status/{memo_id}
    Relayer->>DB: Read intent + forwards
    Relayer-->>API: completed
    API-->>User: Deposit completed
```

## 3. Deposit Forwarding Decision Flow

This shows how each inbound payment is classified.

```mermaid
flowchart TD
    A["Inbound payment on pool account"] --> B["Read memo from Horizon event"]
    B --> C{"Memo present?"}

    C -- "No" --> R1["Sweep to recovery address"]
    C -- "Yes" --> D{"Memo type accepted?"}

    D -- "Hash / return / unsupported" --> R1
    D -- "ID or text" --> E{"Parses as uint64?"}

    E -- "No" --> R1
    E -- "Yes" --> F["Lookup intent by memo_id"]

    F --> G{"Intent exists?"}
    G -- "No" --> R1
    G -- "Yes" --> H{"Intent pending and not expired?"}

    H -- "No" --> R1
    H -- "Yes" --> I["Create or reuse forward record by tx_hash"]

    I --> J["Build Soroban SAC transfer"]
    J --> K["Simulate via Stellar RPC"]
    K --> L["Apply Soroban data and resource fee"]
    L --> M["Sign with pool private key"]
    M --> N["Submit via Horizon"]

    N --> O{"Submit succeeds?"}
    O -- "Yes" --> P["Mark forward done"]
    P --> Q["Mark intent completed"]

    O -- "No" --> S{"Quick retries exhausted?"}
    S -- "No" --> J
    S -- "Yes" --> T["Mark forward pending_retry"]
    T --> U["Retry worker attempts later"]
    U --> V{"Final retry succeeds?"}
    V -- "Yes" --> P
    V -- "No" --> W["Mark forward failed"]
    W --> X["Mark intent failed"]

    R1 --> R2["Record failed forward with reason"]
```

## 4. Intent Lifecycle

This shows the state machine for funding intents.

```mermaid
stateDiagram-v2
    [*] --> pending: POST /intents
    pending --> completed: Deposit forwarded successfully
    pending --> expired: TTL passes before valid deposit
    pending --> failed: Forward retries exhausted
    completed --> [*]
    expired --> [*]
    failed --> [*]
```

## 5. Forward Record Lifecycle

This shows how inbound payment records move through the retry path.

```mermaid
stateDiagram-v2
    [*] --> pending: Payment event inserted
    pending --> done: Forward succeeds
    pending --> pending_retry: Quick retries exhausted
    pending_retry --> done: Retry worker succeeds
    pending_retry --> failed: Final retry fails
    pending --> failed: Invalid / unknown / expired memo swept
    done --> [*]
    failed --> [*]
```

## 6. Database Model

This captures the three core tables and their operational relationships.

```mermaid
erDiagram
    INTENTS {
        uuid id PK
        bigint memo_id UK
        text c_address
        text pool_address
        text expected_amt
        timestamptz expires_at
        text status
        text external_id
        timestamptz created_at
        timestamptz updated_at
    }

    FORWARDS {
        bigserial id PK
        text tx_hash UK
        bigint memo_id
        text from_address
        text amount
        text asset
        text forward_tx
        text status
        int retries
        text error
        timestamptz created_at
        timestamptz updated_at
    }

    CURSORS {
        text pool_address PK
        text cursor
        timestamptz updated_at
    }

    INTENTS ||--o{ FORWARDS : "memo_id"
    CURSORS ||--o{ INTENTS : "pool_address"
```

## 7. Runtime Workers

This shows the internal shape of the Go process.

```mermaid
flowchart TB
    subgraph Process["latch-relayer process"]
        HTTP["HTTP server<br/>/intents, /deposit/status, /health"]
        Store["Store layer"]
        Forwarder["Forwarder service"]
        Retry["Retry worker<br/>every 30s"]
        Watcher1["Watcher goroutine<br/>POOL_ADDRESS_1"]
        WatcherN["Watcher goroutine<br/>POOL_ADDRESS_N"]
    end

    DB[("PostgreSQL")]
    Horizon["Stellar Horizon"]
    RPC["Stellar RPC"]

    HTTP --> Store
    Store --> DB

    Watcher1 -->|"StreamPayments with cursor"| Horizon
    WatcherN -->|"StreamPayments with cursor"| Horizon
    Watcher1 --> Store
    WatcherN --> Store
    Watcher1 --> Forwarder
    WatcherN --> Forwarder

    Retry --> Store
    Retry --> Forwarder
    Retry -->|"Expire stale intents"| Store

    Forwarder --> Store
    Forwarder -->|"Simulate transaction"| RPC
    Forwarder -->|"Submit transaction"| Horizon
```

## 8. Soroban SAC Forwarding

This explains why forwarding to a C-address is a Soroban invocation rather than a classic Stellar payment.

```mermaid
sequenceDiagram
    autonumber
    participant Forwarder
    participant Store as PostgreSQL
    participant RPC as Stellar RPC
    participant Horizon as Stellar Horizon
    participant Pool as Pool G-address
    participant SAC as Native XLM SAC
    participant CAddr as C-address

    Forwarder->>Store: Load intent and forward record
    Forwarder->>Forwarder: Build InvokeHostFunction transfer
    Note over Forwarder: transfer(from = pool, to = C-address, amount)
    Forwarder->>RPC: SimulateTransaction
    RPC-->>Forwarder: SorobanData + MinResourceFee
    Forwarder->>Forwarder: Apply simulation result to transaction XDR
    Forwarder->>Pool: Sign transaction with pool keypair
    Forwarder->>Horizon: Submit transaction
    Horizon->>SAC: Invoke transfer
    SAC->>CAddr: Move XLM balance to C-address
    Horizon-->>Forwarder: Transaction hash
    Forwarder->>Store: Save forward_tx and mark done
```

## 9. Invalid Deposit Recovery

This isolates the recovery path for deposits that cannot safely be matched to an active intent.

```mermaid
flowchart TD
    A["Inbound payment received"] --> B{"Valid memo_id?"}
    B -- "No memo" --> R["Sweep to recovery G-address"]
    B -- "Unsupported memo type" --> R
    B -- "Not a uint64" --> R
    B -- "Valid uint64" --> C{"Intent found?"}
    C -- "No" --> R
    C -- "Yes" --> D{"Intent active?"}
    D -- "Expired / completed / failed" --> R
    D -- "Pending" --> F["Forward to C-address"]

    R --> G["Record forward as failed"]
    G --> H["Store error reason for audit"]
    F --> I["Record forward as done"]
    I --> J["Mark intent completed"]
```

## Suggested Reading Order

1. System Context
2. Happy Path Sequence
3. Deposit Forwarding Decision Flow
4. Intent Lifecycle and Forward Record Lifecycle
5. Database Model
6. Runtime Workers
7. Soroban SAC Forwarding
8. Invalid Deposit Recovery
