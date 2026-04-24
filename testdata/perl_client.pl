#!/usr/bin/env perl
use strict;
use warnings;
use Data::ReqRep::Shared::Client;

my ($path, @msgs) = @ARGV;
die "usage: perl_client.pl <path> [msg ...]\n" unless defined $path && @msgs;

my $cli = Data::ReqRep::Shared::Client->new($path);

$| = 1;
for my $msg (@msgs) {
    my $resp = $cli->req_wait($msg, 5.0);
    die "timeout waiting for '$msg'\n" unless defined $resp;
    print "$resp\n";
}
