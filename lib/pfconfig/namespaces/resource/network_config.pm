package pfconfig::namespaces::resource::network_config;

=head1 NAME

pfconfig::namespaces::resource::network_config

=cut

=head1 DESCRIPTION

pfconfig::namespaces::resource::network_config

=cut

use strict;
use warnings;
use NetAddr::IP;
use pf::util;

use base 'pfconfig::namespaces::resource';
use pfconfig::namespaces::config::Network;
use pfconfig::namespaces::interfaces;

sub init {
    my ($self) = @_;

    $self->{networks} = $self->{cache}->get_cache('config::Network');
    $self->{interfaces} = $self->{cache}->get_cache('interfaces');

}

sub build {
    my ($self) = @_;

    my %ConfigNetwork;

    foreach my $network ( keys %{$self->{networks}} ) {
        $ConfigNetwork{$network} = $self->{networks}{$network};
        foreach my $interface (@{$self->{interfaces}{'internal_nets'} // [] }) {
            my $ipe = $interface->tag("vip") || $interface->tag("ip");
            my $net_addr = NetAddr::IP->new($ipe,$interface->mask());
            if ( defined($self->{networks}{$network}{'next_hop'})) {
                my $ip = new NetAddr::IP::Lite clean_ip($self->{networks}{$network}{'next_hop'});
                if ($net_addr->contains($ip)) {
                    $ConfigNetwork{$network}{'interface'} = $interface;
                }
            } else {
                my $ip = new NetAddr::IP::Lite clean_ip($self->{networks}{$network}{'gateway'});
                if ($net_addr->contains($ip)) {
                    $ConfigNetwork{$network}{'interface'} = $interface;
                }
            }
        }
    }
    return \%ConfigNetwork;
}

=head1 AUTHOR

Inverse inc. <info@inverse.ca>

=head1 COPYRIGHT

Copyright (C) 2005-2017 Inverse inc.

=head1 LICENSE

This program is free software; you can redistribute it and/or
modify it under the terms of the GNU General Public License
as published by the Free Software Foundation; either version 2
of the License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program; if not, write to the Free Software
Foundation, Inc., 51 Franklin Street, Fifth Floor, Boston, MA  02110-1301,
USA.

=cut

1;

# vim: set shiftwidth=4:
# vim: set expandtab:
# vim: set backspace=indent,eol,start:

