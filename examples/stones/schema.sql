-- docsync example: a fictional stone inventory.
--
-- One document per stone. Related data reaches stones in every way docsync supports:
--   inclusion, rate      direct children (row holds stoneid; not part of the primary key)
--   lab                  shared lookup referenced by stone.lab_code (not lab's primary key)
--   order_stone          bridge whose primary key contains stoneid
--   order_header         reached through order_stone
--   order_line           three hops away: order_line -> order_header -> order_stone -> stone
--
-- Run in an empty database. Statements are separated by GO lines.

ALTER DATABASE CURRENT SET ALLOW_SNAPSHOT_ISOLATION ON;
GO
ALTER DATABASE CURRENT SET CHANGE_TRACKING = ON (CHANGE_RETENTION = 2 DAYS, AUTO_CLEANUP = ON);
GO

CREATE TABLE dbo.lab (
    lab_id   int           NOT NULL PRIMARY KEY,
    code     varchar(10)   NOT NULL,
    name     nvarchar(100) NOT NULL,
    country  nvarchar(50)  NULL
);
CREATE UNIQUE INDEX ux_lab_code ON dbo.lab (code);
GO

CREATE TABLE dbo.stone (
    stoneid      bigint        NOT NULL PRIMARY KEY,
    packet_no    varchar(20)   NOT NULL,
    carat        decimal(10,3) NOT NULL,
    shape        varchar(20)   NOT NULL,
    color        varchar(5)    NULL,
    clarity      varchar(5)    NULL,
    lab_code     varchar(10)   NULL,
    status       varchar(20)   NOT NULL,
    certified_on date          NULL,
    updated_at   datetime2(3)  NOT NULL
);
CREATE INDEX ix_stone_lab ON dbo.stone (lab_code);
GO

CREATE TABLE dbo.inclusion (
    inclusion_id bigint      NOT NULL PRIMARY KEY,
    stoneid      bigint      NOT NULL,
    kind         varchar(20) NOT NULL,
    position     int         NOT NULL,
    severity     tinyint     NOT NULL
);
CREATE INDEX ix_inclusion_stone ON dbo.inclusion (stoneid);
GO

CREATE TABLE dbo.rate (
    rate_id   bigint        NOT NULL PRIMARY KEY,
    stoneid   bigint        NOT NULL,
    rate_type varchar(10)   NOT NULL,
    amount    decimal(18,2) NOT NULL,
    currency  char(3)       NOT NULL
);
CREATE INDEX ix_rate_stone ON dbo.rate (stoneid);
GO

CREATE TABLE dbo.order_header (
    order_id   bigint        NOT NULL PRIMARY KEY,
    party      nvarchar(100) NOT NULL,
    order_date date          NOT NULL,
    status     varchar(20)   NOT NULL
);
GO

CREATE TABLE dbo.order_stone (
    order_id bigint NOT NULL,
    stoneid  bigint NOT NULL,
    PRIMARY KEY (order_id, stoneid)
);
CREATE INDEX ix_order_stone_stone ON dbo.order_stone (stoneid);
GO

CREATE TABLE dbo.order_line (
    line_id  bigint        NOT NULL PRIMARY KEY,
    order_id bigint        NOT NULL,
    note     nvarchar(200) NULL,
    amount   decimal(18,2) NOT NULL
);
CREATE INDEX ix_order_line_order ON dbo.order_line (order_id);
GO

ALTER TABLE dbo.lab          ENABLE CHANGE_TRACKING;
ALTER TABLE dbo.stone        ENABLE CHANGE_TRACKING;
ALTER TABLE dbo.inclusion    ENABLE CHANGE_TRACKING;
ALTER TABLE dbo.rate         ENABLE CHANGE_TRACKING;
ALTER TABLE dbo.order_header ENABLE CHANGE_TRACKING;
ALTER TABLE dbo.order_stone  ENABLE CHANGE_TRACKING;
ALTER TABLE dbo.order_line   ENABLE CHANGE_TRACKING;
GO
