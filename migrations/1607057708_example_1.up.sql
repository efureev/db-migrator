CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

CREATE TABLE test_users
(
    id         uuid           default uuid_generate_v4() primary key,
    login      varchar(15)  NOT NULL,
    email       varchar(100) NOT NULL,

    created_at timestamp NOT NULL
);